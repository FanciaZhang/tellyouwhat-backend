package platformops

import (
	"context"
	"time"
)

type Cost struct {
	AppID         string `json:"appID"`
	SettledNanos  int64  `json:"settledNanos"`
	ReservedNanos int64  `json:"reservedNanos"`
	UnknownNanos  int64  `json:"unknownNanos"`
	Pending       int64  `json:"pending"`
}
type Activity struct {
	AppID               string  `json:"appID"`
	Operation           string  `json:"operation"`
	Model               string  `json:"model"`
	Calls               int64   `json:"calls"`
	Succeeded           int64   `json:"succeeded"`
	Failed              int64   `json:"failed"`
	Unobserved          int64   `json:"unobserved"`
	AverageMilliseconds float64 `json:"averageMilliseconds"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
}
type Queue struct {
	AppID         string `json:"appID"`
	Operation     string `json:"operation"`
	Status        string `json:"status"`
	Count         int64  `json:"count"`
	OldestSeconds int64  `json:"oldestSeconds"`
}
type Rejection struct {
	AppID     string `json:"appID"`
	Operation string `json:"operation"`
	Reason    string `json:"reason"`
	Count     int64  `json:"count"`
}
type Metrics struct {
	AsOf        time.Time   `json:"asOf"`
	MonthStart  time.Time   `json:"monthStart"`
	WindowStart time.Time   `json:"windowStart"`
	Costs       []Cost      `json:"costs"`
	Activity    []Activity  `json:"activity"`
	Queues      []Queue     `json:"queues"`
	Rejections  []Rejection `json:"rejections"`
}

func (s Store) RecordRejection(ctx context.Context, app, operation, reason string, now time.Time) error {
	valid := false
	operation = Operation(app, operation)
	for _, op := range Operations(app) {
		valid = valid || op == operation
	}
	if !valid {
		return ErrInvalid
	}
	switch reason {
	case "paused", "quota", "budget", "concurrency":
	default:
		return ErrInvalid
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO platform_ops_rejections(hour_start,app_id,operation,reason,total) VALUES(?,?,?,?,1) ON DUPLICATE KEY UPDATE total=total+1`, now.UTC().Truncate(time.Hour), app, operation, reason)
	return err
}

func (s Store) Metrics(ctx context.Context, now time.Time) (Metrics, error) {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := Metrics{AsOf: now, MonthStart: start, WindowStart: now.Add(-24 * time.Hour), Costs: []Cost{}, Activity: []Activity{}, Queues: []Queue{}, Rejections: []Rejection{}}
	// One snapshot keeps every monetary category consistent during settlements.
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT app_id,COALESCE(SUM(CASE WHEN status='settled' THEN actual_nanos ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='pending' AND lease_expires_at>? THEN reserved_nanos ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='unknown' OR (status='pending' AND lease_expires_at<=?) THEN reserved_nanos ELSE 0 END),0),COALESCE(SUM(status='pending' AND lease_expires_at>?),0) FROM ai_cost_attempts WHERE month_start=? GROUP BY app_id`, now, now, now, start.Format("2006-01-02"))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r Cost
		if err = rows.Scan(&r.AppID, &r.SettledNanos, &r.ReservedNanos, &r.UnknownNanos, &r.Pending); err != nil {
			rows.Close()
			return out, err
		}
		out.Costs = append(out.Costs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT app_id,operation,model_name,COUNT(*),COALESCE(SUM(outcome='success'),0),COALESCE(SUM(outcome='error'),0),SUM(outcome IS NULL),COALESCE(AVG(latency_ms),0),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0) FROM ai_cost_attempts WHERE created_at>=? GROUP BY app_id,operation,model_name ORDER BY app_id,operation,model_name`, out.WindowStart)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r Activity
		if err = rows.Scan(&r.AppID, &r.Operation, &r.Model, &r.Calls, &r.Succeeded, &r.Failed, &r.Unobserved, &r.AverageMilliseconds, &r.InputTokens, &r.OutputTokens); err != nil {
			rows.Close()
			return out, err
		}
		out.Activity = append(out.Activity, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT app_id,operation,status,COUNT(*),GREATEST(0,TIMESTAMPDIFF(SECOND,MIN(created_at),?)) FROM ai_jobs WHERE expires_at>? AND status IN ('queued','running') GROUP BY app_id,operation,status`, now, now)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r Queue
		if err = rows.Scan(&r.AppID, &r.Operation, &r.Status, &r.Count, &r.OldestSeconds); err != nil {
			rows.Close()
			return out, err
		}
		out.Queues = append(out.Queues, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT app_id,operation,reason,SUM(total) FROM platform_ops_rejections WHERE hour_start>=? GROUP BY app_id,operation,reason`, out.WindowStart.Truncate(time.Hour).Add(time.Hour))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r Rejection
		if err = rows.Scan(&r.AppID, &r.Operation, &r.Reason, &r.Count); err != nil {
			rows.Close()
			return out, err
		}
		out.Rejections = append(out.Rejections, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}
