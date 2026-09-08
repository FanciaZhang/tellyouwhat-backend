package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/platformops"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
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

func TestAutomaticProtectionAfterClaimPreservesAttemptAndQuota(t *testing.T) {
	ctx := context.Background()
	reserved := contracts.ReservationTokens(jobRequest())
	store, job, limiter := newBudgetedJob(t, reserved, reserved)
	blocked := true
	model := observingJobProvider{complete: func(ctx context.Context) (providerapi.Response, error) {
		if blocked {
			return providerapi.Response{}, costcontrol.ErrProtectionActive
		}
		return fixedJobProvider{}.Complete(ctx, job.Request)
	}}
	worker := NewWorker(store, model, limiter)
	for i := 0; i < 8; i++ {
		if err := worker.Process(ctx, job.ID); !errors.Is(err, ErrAdmissionDeferred) {
			t.Fatalf("deferral: %v", err)
		}
		got, err := store.Get(ctx, job.ID)
		if err != nil || got.Status != StatusQueued || got.AttemptCount != 0 || !got.ExpiresAt.Equal(job.ExpiresAt) {
			t.Fatalf("lost deferred job: %+v %v", got, err)
		}
		snapshot, err := limiter.Snapshot(ctx, job.OwnerTransactionID, time.Now())
		if err != nil || snapshot.DailyUsed != reserved {
			t.Fatalf("repeated admission consumed extra quota: %+v %v", snapshot, err)
		}
	}
	blocked = false
	if err := worker.Process(ctx, job.ID); err != nil {
		t.Fatal("resume failed", err)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil || got.Status != StatusSucceeded || got.AttemptCount != 1 {
		t.Fatalf("resume: %+v %v", got, err)
	}
	snapshot, err := limiter.Snapshot(ctx, job.OwnerTransactionID, time.Now())
	if err != nil || snapshot.DailyUsed != got.InputTokens+got.OutputTokens {
		t.Fatalf("actual settlement: %+v %v", snapshot, err)
	}
}

func TestAutomaticProtectionCannotResurrectCancelledJob(t *testing.T) {
	ctx := context.Background()
	reserved := contracts.ReservationTokens(jobRequest())
	store, job, limiter := newBudgetedJob(t, reserved, reserved)
	model := observingJobProvider{complete: func(ctx context.Context) (providerapi.Response, error) {
		if err := store.Cancel(ctx, job.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
		return providerapi.Response{}, costcontrol.ErrProtectionActive
	}}
	if err := NewWorker(store, model, limiter).Process(ctx, job.ID); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("cancelled admission: %v", err)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil || got.Status != StatusCancelled {
		t.Fatal("cancelled job resurrected", err)
	}
}
