package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/platformops"
)

func (s *Server) operationsFailure(ctx context.Context, operation, requestID string) *apiFailure {
	err := platformops.Check(ctx, s.operations, string(s.app.ID), operation)
	if err == nil {
		return nil
	}
	if errors.Is(err, platformops.ErrPaused) {
		reason := "paused"
		if errors.Is(err, costcontrol.ErrProtectionActive) {
			reason = "automatic_protection"
		}
		s.recordOperationRejection(ctx, operation, reason)
		return newAPIFailure(503, "ai_paused", "this cloud function is temporarily paused", requestID)
	}
	return newAPIFailure(503, "operations_unavailable", "cloud configuration is unavailable", requestID)
}
func (s *Server) recordOperationRejection(ctx context.Context, operation, reason string) {
	if recorder, ok := s.operations.(costcontrol.RejectionRecorder); ok {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = recorder.RecordRejection(work, string(s.app.ID), operation, reason, s.now())
	}
}
