package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
	"github.com/tellyouwhat/backend/internal/quota"
)

func TestWorkerAccountsForEveryProviderAttempt(t *testing.T) {
	for _, test := range []struct {
		name          string
		failures      int
		usage         int
		wantStatus    Status
		invalidResult bool
	}{
		{"first_attempt_succeeds", 0, 8, StatusSucceeded, false},
		{"retry_succeeds", 1, 8, StatusSucceeded, false},
		{"third_attempt_succeeds", 2, 8, StatusSucceeded, false},
		{"retry_succeeds_without_usage", 1, 0, StatusSucceeded, false},
		{"third_attempt_succeeds_without_usage", 2, 0, StatusSucceeded, false},
		{"retry_has_usage_above_reservation", 1, 100_000, StatusSucceeded, false},
		{"all_attempts_fail", 3, 0, StatusFailed, false},
		{"retry_has_invalid_structured_result", 1, 8, StatusFailed, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			request := jobRequest()
			reserved := contracts.ReservationTokens(request)
			principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1", TransactionID: "transaction-1"}
			digest := "retry-quota-fixture"
			limiter := quota.NewMemoryLimiter(quota.Limits{DailyTokensPerTransaction: 1_000_000, MonthlyTokensPerTransaction: 5_000_000})
			lease, err := limiter.Acquire(ctx, quota.Identity{DeviceID: principal.DeviceID, TransactionID: principal.TransactionID, IP: "203.0.113.1"}, request.Operation, reserved, quota.JobReservationID(principal.KeyID, request.RequestID, digest), now)
			if err != nil {
				t.Fatal(err)
			}
			lease.Release(reserved)
			store := NewMemoryStore()
			service := NewService(store, func() time.Time { return now })
			job, err := service.Enqueue(ctx, principal, request, digest)
			if err != nil {
				t.Fatal(err)
			}
			model := &retryQuotaProvider{failures: test.failures, usage: test.usage, invalidResult: test.invalidResult}
			worker := NewWorker(store, model, limiter)
			attempts := test.failures + 1
			if attempts > maximumAttempts {
				attempts = maximumAttempts
			}
			for attempt := 1; attempt <= attempts; attempt++ {
				err := worker.Process(ctx, job.ID)
				if (err != nil) != (attempt <= test.failures || test.invalidResult) {
					t.Fatalf("attempt %d: %v", attempt, err)
				}
			}
			result, err := service.Get(ctx, principal, job.ID)
			if err != nil || result.Status != test.wantStatus || model.calls != attempts {
				t.Fatalf("incorrect terminal state or number of calls: result=%+v calls=%d err=%v", result, model.calls, err)
			}
			want := reserved * attempts
			if test.wantStatus == StatusSucceeded && test.usage > 0 {
				want = reserved*(attempts-1) + test.usage
			}
			snapshot, err := limiter.Snapshot(ctx, principal.TransactionID, now)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.DailyUsed != want || snapshot.MonthlyUsed != want {
				t.Errorf("quota = %+v, want %d; prior provider attempts must not disappear from accounting", snapshot, want)
			}
			// A duplicate queue delivery must neither call the provider nor charge again.
			if err := worker.Process(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			duplicate, err := limiter.Snapshot(ctx, principal.TransactionID, now)
			if err != nil || duplicate.DailyUsed != snapshot.DailyUsed || model.calls != attempts {
				t.Fatalf("duplicate delivery repeated billing or provider call: usage=%+v calls=%d err=%v", duplicate, model.calls, err)
			}
		})
	}
}

type retryQuotaProvider struct {
	fixedJobProvider
	failures      int
	usage         int
	calls         int
	invalidResult bool
}

func (model *retryQuotaProvider) Complete(context.Context, contracts.Request) (providerapi.Response, error) {
	model.calls++
	if model.calls <= model.failures {
		return providerapi.Response{}, errors.New("synthetic upstream failure")
	}
	content := `{"choice":"soup"}`
	if model.invalidResult {
		content = `{"choice":12}`
	}
	return providerapi.Response{Content: content, InputTokens: model.usage}, nil
}
