package simulatortest

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/capability"
	"github.com/tellyouwhat/backend/internal/media"
)

// Organizing rejects every reused request ID. Keep simulator attempts in the
// existing durable nonce store without manufacturing Apple key or media rows.
type MediaRegistry struct {
	media.Registry
	Simulator *Service
	Uses      capability.UseStore
}

func (r MediaRegistry) CommitAttempt(ctx context.Context, expected []media.Record, attempt media.AttemptRecord, now time.Time) (media.AttemptRecord, bool, error) {
	if !IsKey(attempt.OwnerKeyID) {
		return r.Registry.CommitAttempt(ctx, expected, attempt, now)
	}
	id, err := uuid.Parse(attempt.RequestID)
	digest, digestErr := hex.DecodeString(attempt.BodyDigest)
	if len(expected) != 0 || r.Simulator == nil || !r.Simulator.AllowsVoice(attempt.OwnerKeyID) ||
		err != nil || id.String() != attempt.RequestID || digestErr != nil || len(digest) != 32 ||
		!now.Before(attempt.ExpiresAt) || attempt.ExpiresAt.Sub(now) > 24*time.Hour {
		return media.AttemptRecord{}, false, media.ErrNotAuthorized
	}
	if r.Uses == nil {
		return media.AttemptRecord{}, false, media.ErrUnavailable
	}
	consumed, err := r.Uses.Consume(ctx, "journal-development-organization:"+attempt.RequestID, attempt.ExpiresAt, now)
	if err != nil {
		return media.AttemptRecord{}, false, media.ErrUnavailable
	}
	if !consumed {
		return media.AttemptRecord{}, false, media.ErrIdempotencyConflict
	}
	return attempt, false, nil
}
