package mysqlstore

import (
	"context"
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
	_, err := s.database.ExecContext(ctx, `UPDATE ai_cost_attempts SET outcome=?,latency_ms=GREATEST(0,TIMESTAMPDIFF(MICROSECOND,created_at,?)) DIV 1000,input_tokens=?,output_tokens=?,model_name=?,usage_known=? WHERE id=? AND outcome IS NULL`, state, now.UTC(), o.InputTokens, o.OutputTokens, o.Model, o.UsageKnown, id)
	return err
}
func (s *CostControlStore) RecordRejection(ctx context.Context, app, operation, reason string, now time.Time) error {
	return (platformops.Store{DB: s.database}).RecordRejection(ctx, app, operation, reason, now)
}
