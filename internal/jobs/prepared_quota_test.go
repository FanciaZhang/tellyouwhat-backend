package jobs

import (
	"context"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/quota"
	"testing"
	"time"
)

func TestPreparedJobCancellationDoesNotConsumeQuota(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "after_reservation"}[race], func(t *testing.T) {
			ctx := context.Background()
			store := NewMemoryStore()
			service := NewService(store, time.Now)
			principal := attestation.Principal{KeyID: "key", DeviceID: "device", TransactionID: "transaction"}
			job, err := service.Enqueue(ctx, principal, jobRequest(), "digest")
			if err != nil {
				t.Fatal(err)
			}
			limiter := quota.NewMemoryLimiter(quota.Limits{DailyTokensPerTransaction: 300000, MonthlyTokensPerTransaction: 5000000})
			err = limiter.PrepareJob(ctx, quota.Identity{DeviceID: principal.DeviceID, TransactionID: principal.TransactionID, IP: "203.0.113.1"}, job.Request.Operation, contracts.ReservationTokens(job.Request), quota.JobReservationID(principal.KeyID, job.RequestID, job.BodyDigest), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			budget := &interruptedAttemptBudget{JobAttemptBudget: limiter}
			cancel := func() {
				if err := service.Cancel(ctx, principal, job.ID); err != nil {
					t.Fatal(err)
				}
			}
			if race {
				budget.afterReserve = cancel
			} else {
				cancel()
			}
			model := &retryQuotaProvider{usage: 8}
			_ = NewWorker(store, model, budget).Process(ctx, job.ID)
			if model.calls != 0 {
				t.Fatalf("cancelled task called provider %d times", model.calls)
			}
			snapshot, _ := limiter.Snapshot(ctx, principal.TransactionID, time.Now())
			if snapshot.DailyUsed != 0 || snapshot.MonthlyUsed != 0 {
				t.Fatalf("unexecuted cancellation spent tokens: %+v", snapshot)
			}
		})
	}
}
