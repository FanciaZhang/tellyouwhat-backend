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

func newBudgetedJob(t *testing.T, daily, monthly int) (*MemoryStore, Job, *quota.MemoryLimiter) {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	job, err := NewService(store, time.Now).Enqueue(ctx, attestation.Principal{
		KeyID: "key", DeviceID: "device", TransactionID: "transaction",
	}, jobRequest(), "attempt-budget-fixture")
	if err != nil {
		t.Fatal(err)
	}
	limiter := quota.NewMemoryLimiter(quota.Limits{DailyTokensPerTransaction: daily, MonthlyTokensPerTransaction: monthly})
	prepayJob(t, limiter, job)
	return store, job, limiter
}

func prepayJob(t *testing.T, limiter *quota.MemoryLimiter, job Job) {
	t.Helper()
	owner := job.OwnerTransactionID
	if owner == "" {
		owner = job.OwnerKeyID
	}
	reserved := contracts.ReservationTokens(job.Request)
	lease, err := limiter.Acquire(context.Background(), quota.Identity{
		DeviceID: job.OwnerDeviceID, TransactionID: owner, IP: "203.0.113.1",
	}, job.Request.Operation, reserved, quota.JobReservationID(job.OwnerKeyID, job.RequestID, job.BodyDigest), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	lease.Release(reserved)
}

type observingJobProvider struct {
	fixedJobProvider
	complete func(context.Context) (providerapi.Response, error)
}

func (model observingJobProvider) Complete(ctx context.Context, _ contracts.Request) (providerapi.Response, error) {
	return model.complete(ctx)
}

func TestWorkerReservesQuotaBeforeEachProviderCall(t *testing.T) {
	reserved := contracts.ReservationTokens(jobRequest())
	store, job, limiter := newBudgetedJob(t, reserved*3, reserved*3)
	calls := 0
	model := observingJobProvider{complete: func(ctx context.Context) (providerapi.Response, error) {
		calls++
		snapshot, err := limiter.Snapshot(ctx, job.OwnerTransactionID, time.Now())
		if err != nil || snapshot.DailyUsed != calls*reserved || snapshot.MonthlyUsed != calls*reserved {
			t.Errorf("provider call %d started without its own reservation: %+v err=%v", calls, snapshot, err)
		}
		if calls < 3 {
			return providerapi.Response{}, errors.New("synthetic upstream failure")
		}
		return fixedJobProvider{}.Complete(ctx, job.Request)
	}}
	worker := NewWorker(store, model, limiter)
	for attempt := 1; attempt <= 3; attempt++ {
		err := worker.Process(context.Background(), job.ID)
		if (err != nil) != (attempt < 3) {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	if calls != 3 {
		t.Fatalf("provider calls = %d, want 3", calls)
	}
}

func TestWorkerStopsRetryWhenTokenBudgetExhausted(t *testing.T) {
	reserved := contracts.ReservationTokens(jobRequest())
	for _, scope := range []quota.LimitScope{quota.LimitDailyTokens, quota.LimitMonthlyTokens} {
		t.Run(string(scope), func(t *testing.T) {
			daily, monthly := reserved*10, reserved*10
			if scope == quota.LimitDailyTokens {
				daily = reserved
			} else {
				monthly = reserved
			}
			store, job, limiter := newBudgetedJob(t, daily, monthly)
			model := &retryQuotaProvider{failures: 3}
			worker := NewWorker(store, model, limiter)
			if err := worker.Process(context.Background(), job.ID); err == nil {
				t.Fatal("expected upstream failure")
			}
			err := worker.Process(context.Background(), job.ID)
			if actual, ok := quota.ExceededScope(err); !ok || actual != scope {
				t.Errorf("retry must be denied by %s before a provider call: %v", scope, err)
			}
			if model.calls != 1 {
				t.Fatalf("exhausted budget allowed %d provider calls", model.calls)
			}
		})
	}
}

func TestWorkerKeepsAttemptReservationsAfterCancellation(t *testing.T) {
	reserved := contracts.ReservationTokens(jobRequest())
	store, job, limiter := newBudgetedJob(t, reserved*3, reserved*3)
	calls := 0
	worker := NewWorker(store, observingJobProvider{complete: func(ctx context.Context) (providerapi.Response, error) {
		calls++
		if calls == 2 {
			if err := store.Cancel(ctx, job.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		return providerapi.Response{}, errors.New("synthetic upstream failure")
	}}, limiter)
	for range 2 {
		if err := worker.Process(context.Background(), job.ID); err == nil {
			t.Fatal("expected upstream failure")
		}
	}
	snapshot, err := limiter.Snapshot(context.Background(), job.OwnerTransactionID, time.Now())
	if err != nil || snapshot.DailyUsed != 2*reserved || snapshot.MonthlyUsed != 2*reserved {
		t.Fatalf("cancellation lost attempted usage: %+v err=%v", snapshot, err)
	}
	if err := worker.Process(context.Background(), job.ID); err != nil || calls != 2 {
		t.Fatalf("cancelled job was dispatched again: calls=%d err=%v", calls, err)
	}
}

func TestWorkerMissingBudgetFailsClosed(t *testing.T) {
	store, job, _ := newBudgetedJob(t, 100_000, 1_000_000)
	model := &retryQuotaProvider{usage: 8}
	if err := NewWorker(store, model, nil).Process(context.Background(), job.ID); err == nil || model.calls != 0 {
		t.Fatalf("missing budget allowed a provider call: calls=%d err=%v", model.calls, err)
	}
}

type interruptedAttemptBudget struct {
	quota.JobAttemptBudget
	failBeforeReserve bool
	failAfterReserve  bool
	failSettlement    bool
	afterReserve      func()
}

func (budget *interruptedAttemptBudget) ReserveJobAttempt(ctx context.Context, attempt quota.JobAttempt, now time.Time) (string, error) {
	if budget.failBeforeReserve {
		return "", errors.New("synthetic Redis outage before reservation")
	}
	id, err := budget.JobAttemptBudget.ReserveJobAttempt(ctx, attempt, now)
	if err != nil {
		return "", err
	}
	if budget.afterReserve != nil {
		budget.afterReserve()
	}
	if budget.failAfterReserve {
		return "", errors.New("synthetic lost Redis acknowledgement")
	}
	return id, nil
}

func (budget *interruptedAttemptBudget) Reconcile(ctx context.Context, owner, id string, reserved, actual int, now time.Time) error {
	if budget.failSettlement {
		return errors.New("synthetic Redis outage during settlement")
	}
	return budget.JobAttemptBudget.Reconcile(ctx, owner, id, reserved, actual, now)
}

func TestWorkerDoesNotCallProviderOnUncertainRetryAdmission(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_commit", true: "lost_acknowledgement"}[committed], func(t *testing.T) {
			reserved := contracts.ReservationTokens(jobRequest())
			store, job, limiter := newBudgetedJob(t, reserved*3, reserved*3)
			budget := &interruptedAttemptBudget{JobAttemptBudget: limiter}
			model := &retryQuotaProvider{failures: 1, usage: 8}
			worker := NewWorker(store, model, budget)
			if err := worker.Process(context.Background(), job.ID); err == nil {
				t.Fatal("expected first provider failure")
			}
			budget.failBeforeReserve, budget.failAfterReserve = !committed, committed
			if err := worker.Process(context.Background(), job.ID); err == nil || model.calls != 1 {
				t.Fatalf("uncertain admission started provider: calls=%d err=%v", model.calls, err)
			}
			budget.failBeforeReserve, budget.failAfterReserve = false, false
			if err := worker.Process(context.Background(), job.ID); err != nil || model.calls != 2 {
				t.Fatalf("service recovery failed: calls=%d err=%v", model.calls, err)
			}
			want := reserved + 8
			if committed {
				want += reserved
			}
			snapshot, err := limiter.Snapshot(context.Background(), job.OwnerTransactionID, time.Now())
			if err != nil || snapshot.DailyUsed != want || snapshot.MonthlyUsed != want {
				t.Fatalf("uncertain admission lost its conservative reservation: %+v want=%d err=%v", snapshot, want, err)
			}
		})
	}
}

func TestWorkerChecksJobStateAfterQuotaAdmission(t *testing.T) {
	for _, state := range []string{"cancelled", "expired", "lease_expired", "deleted"} {
		t.Run(state, func(t *testing.T) {
			store, job, limiter := newBudgetedJob(t, 100_000, 1_000_000)
			budget := &interruptedAttemptBudget{JobAttemptBudget: limiter, afterReserve: func() {
				store.mu.Lock()
				defer store.mu.Unlock()
				current := store.jobs[job.ID]
				switch state {
				case "cancelled":
					current.Status = StatusCancelled
				case "expired":
					current.ExpiresAt = time.Now().Add(-time.Second)
				case "lease_expired":
					current.ClaimExpiresAt = time.Now().Add(-time.Second)
				case "deleted":
					delete(store.jobs, job.ID)
					return
				}
				store.jobs[job.ID] = current
			}}
			model := &retryQuotaProvider{usage: 8}
			if err := NewWorker(store, model, budget).Process(context.Background(), job.ID); err == nil || model.calls != 0 {
				t.Fatalf("%s job started provider: calls=%d err=%v", state, model.calls, err)
			}
		})
	}
}

func TestWorkerSettlesKnownUsageEvenWhenResultIsCancelled(t *testing.T) {
	store, job, limiter := newBudgetedJob(t, 100_000, 1_000_000)
	worker := NewWorker(store, observingJobProvider{complete: func(ctx context.Context) (providerapi.Response, error) {
		if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(job.ExpiresAt) {
			t.Errorf("provider context is not bounded by job expiry: %s present=%v", deadline, ok)
		}
		if err := store.Cancel(ctx, job.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
		return providerapi.Response{Content: `{"choice":"soup"}`, InputTokens: 10_000}, nil
	}}, limiter)
	if err := worker.Process(context.Background(), job.ID); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("cancelled result must not become successful: %v", err)
	}
	snapshot, err := limiter.Snapshot(context.Background(), job.OwnerTransactionID, time.Now())
	if err != nil || snapshot.DailyUsed != 10_000 || snapshot.MonthlyUsed != 10_000 {
		t.Fatalf("cancelled persistence discarded measured provider usage: %+v err=%v", snapshot, err)
	}
}

func TestWorkerKeepsEachPrepaymentWhenSettlementFails(t *testing.T) {
	reserved := contracts.ReservationTokens(jobRequest())
	store, job, limiter := newBudgetedJob(t, reserved*3, reserved*3)
	budget := &interruptedAttemptBudget{JobAttemptBudget: limiter, failSettlement: true}
	model := &retryQuotaProvider{failures: 1, usage: 8}
	worker := NewWorker(store, model, budget)
	if err := worker.Process(context.Background(), job.ID); err == nil {
		t.Fatal("expected first provider failure")
	}
	if err := worker.Process(context.Background(), job.ID); err != nil {
		t.Fatalf("valid response was lost to settlement outage: %v", err)
	}
	if err := worker.Process(context.Background(), job.ID); err != nil || model.calls != 2 {
		t.Fatalf("settlement outage dispatched terminal job: calls=%d err=%v", model.calls, err)
	}
	snapshot, err := limiter.Snapshot(context.Background(), job.OwnerTransactionID, time.Now())
	if err != nil || snapshot.DailyUsed != reserved*2 || snapshot.MonthlyUsed != reserved*2 {
		t.Fatalf("settlement outage lost attempt reservations: %+v err=%v", snapshot, err)
	}
}

func TestWorkerCrashRecoveryRequiresAnotherReservation(t *testing.T) {
	reserved := contracts.ReservationTokens(jobRequest())
	store, job, limiter := newBudgetedJob(t, reserved*3, reserved*3)
	ctx := context.Background()
	claimed, err := store.Claim(ctx, job.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.ReserveJobAttempt(ctx, jobAttempt(claimed), time.Now()); err != nil {
		t.Fatal(err)
	}
	// The provider outcome is unknown after a process crash.
	store.mu.Lock()
	claimed.ClaimExpiresAt = time.Now().Add(-time.Second)
	store.jobs[job.ID] = claimed
	store.mu.Unlock()
	worker := NewWorker(store, observingJobProvider{complete: func(ctx context.Context) (providerapi.Response, error) {
		snapshot, err := limiter.Snapshot(ctx, job.OwnerTransactionID, time.Now())
		if err != nil || snapshot.DailyUsed != 2*reserved {
			t.Errorf("crash recovery reused uncertain prepayment: %+v err=%v", snapshot, err)
		}
		return fixedJobProvider{}.Complete(ctx, job.Request)
	}}, limiter)
	if err := worker.Process(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
}
