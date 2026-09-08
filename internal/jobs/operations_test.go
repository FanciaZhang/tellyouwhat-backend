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
