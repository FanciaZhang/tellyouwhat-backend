package platformops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

var ErrProtected = fmt.Errorf("%w: %w", ErrPaused, costcontrol.ErrProtectionActive)

type circuitRecord struct {
	Circuit
	Attempt string `json:"probeAttempt,omitempty"`
}

func readCircuit(ctx context.Context, db queryer, app, op string) (Circuit, error) {
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT document FROM platform_ops_circuits WHERE app_id=? AND operation=?`, app, op).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Circuit{Phase: "closed"}, nil
	}
	if err != nil {
		return Circuit{}, err
	}
	var r circuitRecord
	if err = json.Unmarshal(raw, &r); err != nil {
		return Circuit{}, err
	}
	r.Circuit.ProbeID = r.Attempt
	return r.Circuit, nil
}

func saveCircuit(ctx context.Context, tx *sql.Tx, app, op string, c Circuit, now time.Time) error {
	raw, err := json.Marshal(circuitRecord{Circuit: c, Attempt: c.ProbeID})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO platform_ops_circuits(app_id,operation,document,updated_at) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE document=VALUES(document),updated_at=VALUES(updated_at)`, app, op, raw, now)
	return err
}

func automationOperations(app string) []string {
	if app == "journal" {
		return []string{"journal.organize.lite", "journal.organize.pro", "journal.voice.rewrite", "journal.voice.speech"}
	}
	return Operations(app)
}

func validAutomationOperation(app, op string) bool {
	for _, candidate := range automationOperations(app) {
		if candidate == op {
			return true
		}
	}
	return false
}

func (s Store) CheckAutomation(ctx context.Context, app, op string) error {
	r, err := s.Current(ctx)
	if err != nil {
		return err
	}
	if r.Policy.AutomationRules().Mode != "protect" || !validAutomationOperation(app, op) {
		return nil
	}
	c, err := readCircuit(ctx, s.DB, app, op)
	if err != nil {
		return err
	}
	if !c.Permits(time.Now()) {
		return ErrProtected
	}
	return nil
}

// ReserveAutomation is called under the shared cost-control lock. Its probe
// claim rolls back if normal budget, concurrency, or persistence checks fail.
func ReserveAutomation(ctx context.Context, tx *sql.Tx, p Policy, a costcontrol.Attempt) error {
	if p.AutomationRules().Mode != "protect" || !validAutomationOperation(a.AppID, a.Operation) {
		return nil
	}
	c, err := readCircuit(ctx, tx, a.AppID, a.Operation)
	if err != nil {
		return err
	}
	if !c.Permits(a.CreatedAt) {
		return ErrProtected
	}
	if c.Phase == "probe" {
		c.Claim(a.ID, a.LeaseExpiresAt)
		if err = saveCircuit(ctx, tx, a.AppID, a.Operation, c, a.CreatedAt); err != nil {
			return err
		}
		return automationEvent(ctx, tx, a.AppID, a.Operation, "probe_started", "error_rate", map[string]any{"expiresAt": a.LeaseExpiresAt}, a.CreatedAt)
	}
	return nil
}

// CompleteAutomation runs in the same transaction as the first outcome write.
func CompleteAutomation(ctx context.Context, tx *sql.Tx, id, app, op, result string, now time.Time) error {
	if !validAutomationOperation(app, op) {
		return nil
	}
	r, err := CurrentFrom(ctx, tx)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c, err := readCircuit(ctx, tx, app, op)
	if err != nil {
		return err
	}
	action := c.Advance(r.Policy.AutomationRules(), now)
	if action == "" {
		action = c.Complete(r.Policy.AutomationRules(), id, result, now)
	}
	if action == "" {
		return nil
	}
	if err = saveCircuit(ctx, tx, app, op, c, now); err != nil {
		return err
	}
	if action == "recovered" {
		if err = resolveIncident(ctx, tx, app, op, "error_rate", now); err != nil {
			return err
		}
	}
	return automationEvent(ctx, tx, app, op, action, "error_rate", map[string]any{"successes": c.Successes, "retryAt": c.RetryAt}, now)
}

func automationEvent(ctx context.Context, tx *sql.Tx, app, op, action, rule string, evidence map[string]any, now time.Time) error {
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO platform_ops_events(app_id,operation,action,rule_name,evidence,created_at) VALUES(?,?,?,?,?,?)`, app, op, action, rule, raw, now.UTC())
	return err
}
