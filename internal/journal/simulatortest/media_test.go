package simulatortest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/capability"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/media"
)

type unavailableCommitUses struct{}

func (unavailableCommitUses) Consume(context.Context, string, time.Time, time.Time) (bool, error) {
	return false, errors.New("unavailable")
}

func TestSimulatorOrganizationCommitsUseDurableReplayProtectionWithoutAppleRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	uses := capability.NewMemoryUseStore()
	base := media.NewMemoryRegistry()
	r := MediaRegistry{Registry: base, Simulator: f.s, Uses: uses}
	attempt := media.AttemptRecord{RequestID: uuid.NewString(), OwnerKeyID: f.r.Credentials[0].KeyID,
		BodyDigest: strings.Repeat("a", 64), CreatedAt: f.now, ExpiresAt: f.now.Add(24 * time.Hour)}
	if _, _, err := r.CommitAttempt(ctx, nil, attempt, f.now); err != media.ErrNotAuthorized {
		t.Fatal("unapproved credential could commit", err)
	}
	path := filepath.Join(filepath.Dir(f.s.path), "development-grants.json")
	raw, _ := json.Marshal([]entitlement.DevelopmentGrant{{KeyID: attempt.OwnerKeyID, DeviceID: attempt.OwnerKeyID, StartedAt: f.now, ExpiresAt: f.now.Add(time.Hour)}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.CommitAttempt(ctx, []media.Record{{ObjectID: "not-an-upload-capability"}}, attempt, f.now); err != media.ErrNotAuthorized {
		t.Fatal("simulator gained media access", err)
	}
	got, replay, err := r.CommitAttempt(ctx, nil, attempt, f.now)
	if err != nil || replay || got != attempt {
		t.Fatal("approved text request failed", got, replay, err)
	}
	// Recreating the gateway adapter retains the existing shared durable store.
	r = MediaRegistry{Registry: base, Simulator: f.s, Uses: uses}
	if _, _, err := r.CommitAttempt(ctx, nil, attempt, f.now); err != media.ErrIdempotencyConflict {
		t.Fatal("reused request ID was accepted", err)
	}
	attempt.BodyDigest = strings.Repeat("b", 64)
	if _, _, err := r.CommitAttempt(ctx, nil, attempt, f.now); err != media.ErrIdempotencyConflict {
		t.Fatal("changed body bypassed replay protection", err)
	}
	attempt.RequestID = uuid.NewString()
	r.Uses = nil
	if _, _, err := r.CommitAttempt(ctx, nil, attempt, f.now); err != media.ErrUnavailable {
		t.Fatal("missing durable store failed open", err)
	}
	r.Uses = unavailableCommitUses{}
	if _, _, err := r.CommitAttempt(ctx, nil, attempt, f.now); err != media.ErrUnavailable {
		t.Fatal("durable store failure failed open", err)
	}
	r.Uses = uses
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.CommitAttempt(ctx, nil, attempt, f.now); err != media.ErrNotAuthorized {
		t.Fatal("revocation did not deny a new request", err)
	}
	attempt.OwnerKeyID = "real-apple-key"
	if _, replay, err := r.CommitAttempt(ctx, nil, attempt, f.now); err != nil || replay {
		t.Fatal("ordinary request was not delegated", replay, err)
	}
	if _, replay, err := base.CommitAttempt(ctx, nil, attempt, f.now); err != nil || !replay {
		t.Fatal("ordinary request did not reach its original repository", replay, err)
	}
}
