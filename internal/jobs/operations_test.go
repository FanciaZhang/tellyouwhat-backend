package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/platformops"
	"github.com/tellyouwhat/backend/internal/quota"
)

func TestPausedJobWaitsWithoutUsingAttemptsOrLosingReservation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	service := NewService(store, time.Now)
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}
	job, err := service.Enqueue(ctx, principal, jobRequest(), "digest-1")
	if err != nil {
		t.Fatal(err)
	}
	budget := quota.NewMemoryLimiter(quota.Limits{})
	prepayJob(t, budget, job)
	worker := NewWorker(store, fixedJobProvider{}, budget)
	worker.Admit = func(context.Context, string) error { return platformops.ErrPaused }
	for i := 0; i < 5; i++ {
		if err = worker.Process(ctx, job.ID); !errors.Is(err, platformops.ErrPaused) {
			t.Fatal(err)
		}
	}
	paused, err := store.Get(ctx, job.ID)
	if err != nil || paused.Status != StatusQueued || paused.AttemptCount != 0 {
		t.Fatalf("pause consumed attempt: %+v %v", paused, err)
	}
	worker.Admit = func(context.Context, string) error { return nil }
	if err = worker.Process(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Get(ctx, job.ID)
	if err != nil || completed.Status != StatusSucceeded || completed.AttemptCount != 1 {
		t.Fatalf("resume failed: %+v %v", completed, err)
	}
}

func TestOutboxAdmissionDeferralDoesNotExhaustDeliveries(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	service := NewService(store, func() time.Time { return now })
	job, err := service.Enqueue(ctx, attestation.Principal{KeyID: "key", DeviceID: "device"}, jobRequest(), "digest")
	if err != nil {
		t.Fatal(err)
	}
	paused := true
	dispatcher := dispatchFunction(func(context.Context, string) error {
		if paused {
			return ErrAdmissionDeferred
		}
		return nil
	})
	pump := NewOutboxPump(store, dispatcher, func() time.Time { return now })
	for i := 0; i < 30; i++ {
		if err := pump.drain(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(31 * time.Second)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil || got.Status != StatusQueued || got.AttemptCount != 0 {
		t.Fatalf("deferred: %+v %v", got, err)
	}
	if store.outbox[job.ID].Attempts != 0 {
		t.Fatal("deferral consumed delivery")
	}
	paused = false
	if err = pump.drain(ctx); err != nil {
		t.Fatal(err)
	}
	if store.outbox[job.ID].Attempts != 1 {
		t.Fatal("resume did not deliver")
	}
}

type dispatchFunction func(context.Context, string) error

func (f dispatchFunction) Dispatch(ctx context.Context, id string) error { return f(ctx, id) }
