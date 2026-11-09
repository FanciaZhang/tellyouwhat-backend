package mysqlstore

import (
	"context"
	"database/sql"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/platformops"
)

func (s *CostControlStore) RecordOutcome(ctx context.Context, id string, o costcontrol.Outcome, now time.Time) error {
	if o.InputTokens < 0 || o.OutputTokens < 0 || len(o.Model) > 128 {
		return costcontrol.ErrInvalidAttempt
	}
	state := "error"
	if o.Success {
		state = "success"
	}
	tx, err := s.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockCostControl(ctx, tx); err != nil {
		return err
	}
	var app, op string
	var previous sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT app_id,operation,outcome FROM ai_cost_attempts WHERE id=?`, id).Scan(&app, &op, &previous); err != nil {
		return err
	}
	if previous.Valid {
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `UPDATE ai_cost_attempts SET outcome=?,latency_ms=GREATEST(0,TIMESTAMPDIFF(MICROSECOND,created_at,?)) DIV 1000,input_tokens=?,output_tokens=?,model_name=?,usage_known=?,outcome_recorded_at=?,cancelled=? WHERE id=? AND outcome IS NULL`, state, now.UTC(), o.InputTokens, o.OutputTokens, o.Model, o.UsageKnown, now.UTC(), o.Cancelled, id)
	if err != nil {
		return err
	}
	if o.Cancelled {
		state = "cancelled"
	}
	if err = platformops.CompleteAutomation(ctx, tx, id, app, op, state, now.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *CostControlStore) RecordRejection(ctx context.Context, app, operation, reason string, now time.Time) error {
	return (platformops.Store{DB: s.database}).RecordRejection(ctx, app, operation, reason, now)
}
