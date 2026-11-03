package platformops

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type AutomationCircuit struct {
	AppID         string     `json:"appID"`
	Operation     string     `json:"operation"`
	Phase         string     `json:"phase"`
	BadWindows    int        `json:"badWindows"`
	Successes     int        `json:"successes"`
	ProbeOccupied bool       `json:"probeOccupied"`
	RetryAt       *time.Time `json:"retryAt,omitempty"`
	ProbeUntil    *time.Time `json:"probeUntil,omitempty"`
	ChangedAt     *time.Time `json:"changedAt,omitempty"`
}
type AutomationIncident struct {
	ID         string          `json:"id"`
	AppID      string          `json:"appID"`
	Operation  string          `json:"operation"`
	Rule       string          `json:"rule"`
	OpenedAt   time.Time       `json:"openedAt"`
	LastSeenAt time.Time       `json:"lastSeenAt"`
	ResolvedAt *time.Time      `json:"resolvedAt,omitempty"`
	Evidence   json.RawMessage `json:"evidence"`
}
type AutomationEvent struct {
	ID        int64           `json:"id"`
	AppID     string          `json:"appID"`
	Operation string          `json:"operation"`
	Action    string          `json:"action"`
	Rule      string          `json:"rule"`
	Evidence  json.RawMessage `json:"evidence"`
	CreatedAt time.Time       `json:"createdAt"`
}
type AutomationSnapshot struct {
	LastPatrolAt    *time.Time           `json:"lastPatrolAt,omitempty"`
	PolicyRevision  string               `json:"policyRevision"`
	Host            *HostHealth          `json:"host,omitempty"`
	Circuits        []AutomationCircuit  `json:"circuits"`
	Incidents       []AutomationIncident `json:"incidents"`
	ActiveIncidents int64                `json:"activeIncidents"`
	Events          []AutomationEvent    `json:"events"`
	MoreEvents      bool                 `json:"moreEvents"`
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func readAutomation(ctx context.Context, tx *sql.Tx) (AutomationSnapshot, error) {
	out := AutomationSnapshot{Circuits: []AutomationCircuit{}, Incidents: []AutomationIncident{}, Events: []AutomationEvent{}}
	var last sql.NullTime
	var revision sql.NullString
	var host []byte
	err := tx.QueryRowContext(ctx, `SELECT completed_at,policy_revision,host_document FROM platform_ops_patrol WHERE singleton_id=1`).Scan(&last, &revision, &host)
	if err != nil {
		return out, err
	}
	if last.Valid {
		out.LastPatrolAt = &last.Time
	}
	out.PolicyRevision = revision.String
	if len(host) > 0 {
		out.Host = &HostHealth{}
		if err = json.Unmarshal(host, out.Host); err != nil {
			return out, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT app_id,operation,document FROM platform_ops_circuits ORDER BY app_id,operation`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var app, op string
		var raw []byte
		if err = rows.Scan(&app, &op, &raw); err != nil {
			rows.Close()
			return out, err
		}
		var r circuitRecord
		if err = json.Unmarshal(raw, &r); err != nil {
			rows.Close()
			return out, err
		}
		c := r.Circuit
		out.Circuits = append(out.Circuits, AutomationCircuit{AppID: app, Operation: op, Phase: c.Phase, BadWindows: c.BadWindows, Successes: c.Successes, ProbeOccupied: r.Attempt != "", RetryAt: optionalTime(c.RetryAt), ProbeUntil: optionalTime(c.ProbeUntil), ChangedAt: optionalTime(c.ChangedAt)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM platform_ops_incidents WHERE active_key IS NOT NULL`).Scan(&out.ActiveIncidents); err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,app_id,operation,rule_name,opened_at,last_seen_at,resolved_at,evidence FROM platform_ops_incidents ORDER BY resolved_at IS NULL DESC,opened_at DESC,id DESC LIMIT 100`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r AutomationIncident
		var resolved sql.NullTime
		if err = rows.Scan(&r.ID, &r.AppID, &r.Operation, &r.Rule, &r.OpenedAt, &r.LastSeenAt, &resolved, &r.Evidence); err != nil {
			rows.Close()
			return out, err
		}
		if resolved.Valid {
			r.ResolvedAt = &resolved.Time
		}
		out.Incidents = append(out.Incidents, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,app_id,operation,action,rule_name,evidence,created_at FROM platform_ops_events ORDER BY id DESC LIMIT 101`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r AutomationEvent
		if err = rows.Scan(&r.ID, &r.AppID, &r.Operation, &r.Action, &r.Rule, &r.Evidence, &r.CreatedAt); err != nil {
			rows.Close()
			return out, err
		}
		out.Events = append(out.Events, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Events) > 100 {
		out.MoreEvents = true
		out.Events = out.Events[:100]
	}
	return out, nil
}
