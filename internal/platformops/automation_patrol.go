package platformops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// RunPatrol operates independently of dashboard traffic. A database heartbeat
// deduplicates multiple replicas without keeping a lock between scans.
func (s Store) RunPatrol(ctx context.Context, logger *slog.Logger) {
	run := func() {
		work, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := s.Patrol(work, time.Now()); err != nil && ctx.Err() == nil {
			logger.Warn("operations patrol failed")
		}
	}
	run()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

type condition struct {
	app, op, rule string
	known, bad    bool
	observed      time.Time
	evidence      map[string]any
}

func observeCondition(ctx context.Context, tx *sql.Tx, c condition, now time.Time) error {
	if !c.known {
		return nil
	}
	key := c.rule + ":" + c.app + ":" + c.op
	var id string
	var previous time.Time
	var healthy int
	err := tx.QueryRowContext(ctx, `SELECT id,last_observed_at,healthy_checks FROM platform_ops_incidents WHERE active_key=?`, key).Scan(&id, &previous, &healthy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id != "" && !c.observed.After(previous) {
		return nil
	}
	raw, err := json.Marshal(c.evidence)
	if err != nil {
		return err
	}
	if c.bad {
		if id == "" {
			id = uuid.NewString()
			_, err = tx.ExecContext(ctx, `INSERT INTO platform_ops_incidents(id,active_key,app_id,operation,rule_name,opened_at,last_seen_at,last_observed_at,evidence) VALUES(?,?,?,?,?,?,?,?,?)`, id, key, c.app, c.op, c.rule, now, now, c.observed, raw)
			if err != nil {
				return err
			}
			return automationEvent(ctx, tx, c.app, c.op, "incident_opened", c.rule, c.evidence, now)
		}
		_, err = tx.ExecContext(ctx, `UPDATE platform_ops_incidents SET last_seen_at=?,last_observed_at=?,healthy_checks=0,evidence=? WHERE id=?`, now, c.observed, raw, id)
		return err
	}
	if id == "" {
		return nil
	}
	healthy++
	if healthy >= 2 {
		return resolveIncident(ctx, tx, c.app, c.op, c.rule, now)
	}
	_, err = tx.ExecContext(ctx, `UPDATE platform_ops_incidents SET last_observed_at=?,healthy_checks=? WHERE id=?`, c.observed, healthy, id)
	return err
}

func resolveIncident(ctx context.Context, tx *sql.Tx, app, op, rule string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE platform_ops_incidents SET active_key=NULL,resolved_at=? WHERE active_key=?`, now, rule+":"+app+":"+op)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	return automationEvent(ctx, tx, app, op, "incident_resolved", rule, map[string]any{}, now)
}

type patrolActivity struct {
	completed, failed int64
	average           float64
}

func (s Store) Patrol(ctx context.Context, now time.Time) error {
	now = now.UTC().Truncate(time.Microsecond)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return err
	}
	var previous sql.NullTime
	var hostRaw []byte
	if err = tx.QueryRowContext(ctx, `SELECT completed_at,host_document FROM platform_ops_patrol WHERE singleton_id=1 FOR UPDATE`).Scan(&previous, &hostRaw); err != nil {
		return err
	}
	if previous.Valid && now.Sub(previous.Time) < 45*time.Second {
		return tx.Commit()
	}
	r, err := CurrentFrom(ctx, tx)
	if err != nil {
		return err
	}
	p := r.Policy.AutomationRules()
	end := now.Truncate(p.Window())
	start := end.Add(-p.Window())
	activity := map[string]patrolActivity{}
	rows, err := tx.QueryContext(ctx, `SELECT app_id,operation,COUNT(*),SUM(outcome='error'),COALESCE(AVG(latency_ms),0) FROM ai_cost_attempts WHERE outcome_recorded_at>=? AND outcome_recorded_at<? AND cancelled=FALSE AND outcome IN ('success','error') GROUP BY app_id,operation`, start, end)
	if err != nil {
		return err
	}
	for rows.Next() {
		var app, op string
		var a patrolActivity
		if err = rows.Scan(&app, &op, &a.completed, &a.failed, &a.average); err != nil {
			rows.Close()
			return err
		}
		activity[app+":"+op] = a
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	conditions := []condition{}
	for _, app := range []string{"health", "journal"} {
		for _, op := range automationOperations(app) {
			c, err := readCircuit(ctx, tx, app, op)
			if err != nil {
				return err
			}
			action := c.Advance(p, now)
			if action != "" {
				if err = automationEvent(ctx, tx, app, op, action, "error_rate", map[string]any{"retryAt": c.RetryAt}, now); err != nil {
					return err
				}
			}
			a := activity[app+":"+op]
			known := !start.Before(c.Baseline) && a.completed >= int64(p.MinimumSamples)
			// Speech duration and connection closure are not a model error signal.
			if op != "journal.voice.speech" {
				action = c.Observe(p, AutomationSample{Start: start, End: end, Completed: a.completed, Failed: a.failed}, now)
				if action != "" {
					if err = automationEvent(ctx, tx, app, op, action, "error_rate", map[string]any{"samples": a.completed, "failed": a.failed, "retryAt": c.RetryAt}, now); err != nil {
						return err
					}
				}
				conditions = append(conditions,
					condition{app, op, "error_rate", known, a.failed*100 >= a.completed*int64(r.Policy.ErrorWarningPercent), end, map[string]any{"samples": a.completed, "failed": a.failed, "thresholdPercent": r.Policy.ErrorWarningPercent, "windowStart": start, "windowEnd": end}},
					condition{app, op, "slow_calls", known, a.average >= float64(r.Policy.SlowWarningMilliseconds), end, map[string]any{"samples": a.completed, "averageMilliseconds": a.average, "thresholdMilliseconds": r.Policy.SlowWarningMilliseconds, "windowStart": start, "windowEnd": end}})
			}
			if err = saveCircuit(ctx, tx, app, op, c, now); err != nil {
				return err
			}
		}
	}
	month := now.Format("2006-01") + "-01"
	var total int64
	for _, app := range []string{"health", "journal"} {
		var used int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN status='settled' THEN actual_nanos ELSE reserved_nanos END),0) FROM ai_cost_attempts WHERE app_id=? AND month_start=?`, app, month).Scan(&used); err != nil {
			return err
		}
		total += used
		budget := r.Policy.Apps[app].MonthlyBudgetNanos
		conditions = append(conditions, condition{app, "", "budget", true, budget > 0 && used >= (budget*int64(r.Policy.BudgetWarningPercent)+99)/100, now, map[string]any{"usedNanos": used, "budgetNanos": budget, "month": month}})
		for _, op := range Operations(app) {
			var count, oldest int64
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(TIMESTAMPDIFF(SECOND,created_at,?)),0) FROM ai_jobs WHERE app_id=? AND operation=? AND status='queued' AND expires_at>?`, now, app, op, now).Scan(&count, &oldest); err != nil {
				return err
			}
			conditions = append(conditions, condition{app, op, "queue_backlog", true, count >= int64(p.QueueCount) && oldest >= int64(p.QueueAgeMinutes*60), now, map[string]any{"queued": count, "oldestSeconds": oldest, "thresholdCount": p.QueueCount, "thresholdMinutes": p.QueueAgeMinutes}})
		}
		var unknown int64
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_cost_attempts WHERE app_id=? AND (status='unknown' OR (status='pending' AND lease_expires_at<=?)) AND created_at<=?`, app, now, now.Add(-time.Duration(p.UnknownReservationMinutes)*time.Minute)).Scan(&unknown); err != nil {
			return err
		}
		conditions = append(conditions, condition{app, "", "unknown_cost", true, unknown > 0, now, map[string]any{"attempts": unknown, "thresholdMinutes": p.UnknownReservationMinutes}})
	}
	conditions = append(conditions, condition{"", "", "budget", true, total >= (r.Policy.MonthlyBudgetNanos*int64(r.Policy.BudgetWarningPercent)+99)/100, now, map[string]any{"usedNanos": total, "budgetNanos": r.Policy.MonthlyBudgetNanos, "month": month}})
	var host HostHealth
	hostKnown := len(hostRaw) > 0 && json.Unmarshal(hostRaw, &host) == nil && now.Sub(host.CheckedAt) >= 0 && now.Sub(host.CheckedAt) <= 3*time.Minute
	conditions = append(conditions, condition{"", "", "host_patrol_stale", true, !hostKnown, now, map[string]any{"lastCheckedAt": host.CheckedAt}})
	if hostKnown {
		for _, check := range host.Checks {
			conditions = append(conditions, condition{"", "", check.Name, true, !check.Passed, host.CheckedAt, map[string]any{"ageSeconds": check.AgeSeconds, "freeMegabytes": check.FreeMegabytes}})
		}
	}
	for _, c := range conditions {
		if err = observeCondition(ctx, tx, c, now); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE platform_ops_patrol SET completed_at=?,policy_revision=? WHERE singleton_id=1`, now, r.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
