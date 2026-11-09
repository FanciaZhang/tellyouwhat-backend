package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
)

func TestJobAccessStopsAtExpirationWithoutWaitingForCleanup(t *testing.T) {
	t.Parallel()
	for _, status := range []Status{StatusQueued, StatusRunning, StatusSucceeded, StatusFailed, StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			store := NewMemoryStore()
			now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
			service := NewService(store, func() time.Time { return now })
			principal := attestation.Principal{KeyID: "owner", DeviceID: "device"}
			created, err := service.Enqueue(context.Background(), principal, jobRequest(), "digest")
			if err != nil {
				t.Fatal(err)
			}
			stored := store.jobs[created.ID]
			stored.Status = status
			stored.Result = `{"choice":"synthetic private result"}`
			store.jobs[created.ID] = stored

			now = created.ExpiresAt.Add(-time.Nanosecond)
			before, err := service.Get(context.Background(), principal, created.ID)
			if err != nil || before.Result != stored.Result {
				t.Fatal("unexpired result should remain available")
			}
			for _, boundary := range []time.Time{created.ExpiresAt, created.ExpiresAt.Add(time.Hour)} {
				now = boundary
				expired, err := service.Get(context.Background(), principal, created.ID)
				if !errors.Is(err, ErrNotFound) || expired.ID != "" || expired.Result != "" {
					t.Errorf("expired job was exposed at %s: error=%v, containsResult=%v", boundary, err, expired.Result != "")
				}
			}
			if _, err := store.Get(context.Background(), created.ID); err != nil {
				t.Fatal("this scenario must exercise an expired row awaiting cleanup")
			}
		})
	}
}

func TestExpiredJobCannotBeExposedByIdempotentEnqueue(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	service := NewService(store, func() time.Time { return now })
	principal := attestation.Principal{KeyID: "owner", DeviceID: "device"}
	request := jobRequest()
	created, err := service.Enqueue(context.Background(), principal, request, "digest")
	if err != nil {
		t.Fatal(err)
	}
	stored := store.jobs[created.ID]
	stored.Status = StatusSucceeded
	stored.Result = `{"choice":"synthetic private result"}`
	store.jobs[created.ID] = stored

	now = created.ExpiresAt.Add(-time.Nanosecond)
	reused, err := service.Enqueue(context.Background(), principal, request, "digest")
	if err != nil || reused.ID != created.ID {
		t.Fatal("unexpired retries must stay idempotent")
	}
	now = created.ExpiresAt
	rejected, err := service.EnqueueWithID(context.Background(), principal, "ab14c68c-501a-4fbd-93c3-632f6722a109", request, "digest")
	if !errors.Is(err, ErrIdempotencyConflict) || rejected.ID != "" || rejected.Result != "" {
		t.Fatalf("expired idempotent retry exposed a result: error=%v, containsResult=%v", err, rejected.Result != "")
	}
	if len(store.jobs) != 1 {
		t.Fatal("rejecting an expired retry must not create another job")
	}
}

func TestExpiredJobCancellationDoesNotMutateRetainedState(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	service := NewService(store, func() time.Time { return now })
	principal := attestation.Principal{KeyID: "owner", DeviceID: "device"}
	created, err := service.Enqueue(context.Background(), principal, jobRequest(), "digest")
	if err != nil {
		t.Fatal(err)
	}
	now = created.ExpiresAt
	if err := service.Cancel(context.Background(), principal, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired cancellation should be unavailable, got %v", err)
	}
	retained, err := store.Get(context.Background(), created.ID)
	if err != nil || retained.Status != StatusQueued {
		t.Fatal("expired cancellation changed retained state")
	}
}
