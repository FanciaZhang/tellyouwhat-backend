package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
	"github.com/tellyouwhat/backend/internal/usage"
)

func TestEnqueueIsIdempotentAndRejectsRequestIDDigestConflict(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	service := NewService(store, time.Now)
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}
	request := jobRequest()

	first, err := service.Enqueue(context.Background(), principal, request, "digest-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Enqueue(context.Background(), principal, request, "digest-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotent enqueue returned different jobs: %s != %s", first.ID, second.ID)
	}
	if _, err := service.Enqueue(context.Background(), principal, request, "digest-2"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}

func TestWorkerClaimsJobAndPersistsValidatedResult(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	service := NewService(store, time.Now)
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}
	job, _ := service.Enqueue(context.Background(), principal, jobRequest(), "digest-1")
	worker := NewWorker(store, fixedJobProvider{}, nil)

	if err := worker.Process(context.Background(), job.ID); err != nil {
		t.Fatalf("process: %v", err)
	}
	completed, err := service.Get(context.Background(), principal, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != StatusSucceeded || completed.Result != `{"choice":"soup"}` {
		t.Fatalf("unexpected completed job: %+v", completed)
	}
}

func TestWorkerKeepsDurableSuccessWhenQuotaReconciliationFails(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	service := NewService(store, time.Now)
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}
	job, _ := service.Enqueue(context.Background(), principal, jobRequest(), "digest-1")
	reconciler := &failingTokenReconciler{}
	worker := NewWorker(store, fixedJobProvider{}, reconciler)

	if err := worker.Process(context.Background(), job.ID); err != nil {
		t.Fatalf("durable success must not be redispatched after reconciliation failure: %v", err)
	}
	completed, err := service.Get(context.Background(), principal, job.ID)
	if err != nil || completed.Status != StatusSucceeded || reconciler.calls != 1 {
		t.Fatalf("unexpected completed job or reconciliation count: job=%+v calls=%d err=%v", completed, reconciler.calls, err)
	}
}

func TestHTTPDispatcherDefaultTimeoutCoversWorkerLifetime(t *testing.T) {
	t.Parallel()

	dispatcher := NewHTTPDispatcher("https://worker.test/v1/internal/jobs/process", "secret", "health", nil)
	if dispatcher.client.Timeout != workerDispatchTimeout || dispatcher.client.Timeout <= 3*time.Hour {
		t.Fatalf("default dispatch timeout is shorter than worker lifetime: %s", dispatcher.client.Timeout)
	}
}

func TestStaleRunningLeaseCanBeReclaimedAfterWorkerCrash(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	now := time.Now().UTC()
	service := NewService(store, func() time.Time { return now })
	job, err := service.Enqueue(context.Background(), attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}, jobRequest(), "digest")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(context.Background(), job.ID, now)
	if err != nil || first.AttemptCount != 1 {
		t.Fatalf("first claim: %+v %v", first, err)
	}
	if _, err := store.Claim(context.Background(), job.ID, now.Add(time.Minute)); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("active lease should block duplicate worker, got %v", err)
	}
	reclaimed, err := store.Claim(context.Background(), job.ID, now.Add(claimLeaseDuration+time.Second))
	if err != nil || reclaimed.AttemptCount != 2 {
		t.Fatalf("stale lease was not reclaimed: %+v %v", reclaimed, err)
	}
}

func TestHeartbeatExtensionPreventsLongRunningJobRedispatch(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	now := time.Now().UTC()
	service := NewService(store, func() time.Time { return now })
	job, _ := service.Enqueue(context.Background(), attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}, jobRequest(), "digest")
	claimed, err := store.Claim(context.Background(), job.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	heartbeatAt := now.Add(claimLeaseDuration - time.Second)
	if err := store.ExtendLease(context.Background(), job.ID, claimed.AttemptCount, heartbeatAt); err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimDispatches(context.Background(), now.Add(claimLeaseDuration+time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("active heartbeat lease was redispatched: %+v", items)
	}
}

func TestStaleWorkerCannotFinishReclaimedAttempt(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	now := time.Now().UTC()
	service := NewService(store, func() time.Time { return now })
	job, err := service.Enqueue(context.Background(), attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}, jobRequest(), "digest")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(context.Background(), job.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Claim(context.Background(), job.ID, now.Add(claimLeaseDuration+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	response := providerapi.Response{Content: `{"choice":"soup"}`}
	usageRecord := usage.Record{RequestID: job.RequestID, KeyID: job.OwnerKeyID}
	if err := store.Succeed(context.Background(), job.ID, first.AttemptCount, response, usageRecord, now.Add(claimLeaseDuration+2*time.Second)); !errors.Is(err, ErrJobNotClaimable) {
		t.Fatalf("stale attempt must be fenced, got %v", err)
	}
	if _, exists := store.usage[job.RequestID]; exists {
		t.Fatal("stale attempt wrote usage outside the fenced terminal transaction")
	}
	if err := store.Succeed(context.Background(), job.ID, second.AttemptCount, response, usageRecord, now.Add(claimLeaseDuration+3*time.Second)); err != nil {
		t.Fatalf("current attempt should finish: %v", err)
	}
}

func TestOutboxRemainsDurableUntilJobReachesTerminalState(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	now := time.Now().UTC()
	service := NewService(store, func() time.Time { return now })
	job, _ := service.Enqueue(context.Background(), attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}, jobRequest(), "digest")
	items, err := store.ClaimDispatches(context.Background(), now, 10)
	if err != nil || len(items) != 1 || items[0].JobID != job.ID {
		t.Fatalf("missing transactional dispatch entry: %+v %v", items, err)
	}
	if err := store.CompleteDispatch(context.Background(), job.ID, now); err != nil {
		t.Fatal(err)
	}
	items, _ = store.ClaimDispatches(context.Background(), now.Add(claimLeaseDuration+time.Second), 10)
	if len(items) != 1 || items[0].JobID != job.ID {
		t.Fatalf("unprocessed job was not redispatched: %+v", items)
	}
}

func TestOutboxStopsAfterBoundedAcceptedDeliveriesWithoutWorkerClaim(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	now := time.Now().UTC()
	service := NewService(store, func() time.Time { return now })
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}
	job, _ := service.Enqueue(context.Background(), principal, jobRequest(), "digest")
	for attempt := 0; attempt < 10; attempt++ {
		deliveryAt := now.Add(time.Duration(attempt) * (claimLeaseDuration + time.Second))
		items, err := store.ClaimDispatches(context.Background(), deliveryAt, 1)
		if err != nil || len(items) != 1 {
			t.Fatalf("delivery %d missing: %+v %v", attempt+1, items, err)
		}
		if err := store.CompleteDispatch(context.Background(), job.ID, deliveryAt); err != nil {
			t.Fatal(err)
		}
	}
	finalAt := now.Add(10 * (claimLeaseDuration + time.Second))
	items, err := store.ClaimDispatches(context.Background(), finalAt, 1)
	if err != nil || len(items) != 0 {
		t.Fatalf("delivery should be exhausted: %+v %v", items, err)
	}
	failed, err := service.Get(context.Background(), principal, job.ID)
	if err != nil || failed.Status != StatusFailed || failed.FailureCategory != "worker_delivery_exhausted" {
		t.Fatalf("job did not fail after bounded delivery: %+v %v", failed, err)
	}
}

func TestOutboxDeliveryExhaustionDoesNotKillActiveWorkerLease(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	now := time.Now().UTC()
	service := NewService(store, func() time.Time { return now })
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}
	job, _ := service.Enqueue(context.Background(), principal, jobRequest(), "digest")
	claimed, err := store.Claim(context.Background(), job.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	item := store.outbox[job.ID]
	item.Attempts = 10
	store.outbox[job.ID] = item
	if err := store.RetryDispatch(context.Background(), job.ID, now.Add(time.Second), "worker dispatch rejected"); err != nil {
		t.Fatal(err)
	}
	active, err := service.Get(context.Background(), principal, job.ID)
	if err != nil || active.Status != StatusRunning || active.AttemptCount != claimed.AttemptCount {
		t.Fatalf("active worker was terminated by dispatch exhaustion: %+v %v", active, err)
	}
	if _, exists := store.outbox[job.ID]; !exists {
		t.Fatal("active worker lost durable outbox recovery")
	}
}

func TestExpiredQueuedJobIsFailedAndRemovedFromOutbox(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	now := time.Now().UTC()
	service := NewService(store, func() time.Time { return now })
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}
	job, _ := service.Enqueue(context.Background(), principal, jobRequest(), "digest")
	items, err := store.ClaimDispatches(context.Background(), now.Add(24*time.Hour+time.Second), 1)
	if err != nil || len(items) != 0 {
		t.Fatalf("expired job was dispatched: %+v %v", items, err)
	}
	failed, err := service.Get(context.Background(), principal, job.ID)
	if err != nil || failed.Status != StatusFailed || failed.FailureCategory != "job_expired" {
		t.Fatalf("expired job was not terminally retired: %+v %v", failed, err)
	}
	if _, exists := store.outbox[job.ID]; exists {
		t.Fatal("expired job retained its outbox row")
	}
}

func TestWorkerRetriesUpstreamFailureWithBoundedAttempts(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	service := NewService(store, time.Now)
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}
	job, _ := service.Enqueue(context.Background(), principal, jobRequest(), "digest")
	worker := NewWorker(store, failingJobProvider{}, nil)
	for attempt := 0; attempt < maximumAttempts; attempt++ {
		if err := worker.Process(context.Background(), job.ID); err == nil {
			t.Fatal("failing provider unexpectedly succeeded")
		}
	}
	failed, err := service.Get(context.Background(), principal, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != StatusFailed || failed.AttemptCount != maximumAttempts {
		t.Fatalf("retry policy did not terminate: %+v", failed)
	}
}

func jobRequest() contracts.Request {
	return contracts.Request{
		RequestID:         "19be2f9e-bd92-4699-b561-e3816092114c",
		Operation:         contracts.OperationMealDecision,
		ContractVersion:   contracts.ContractVersionV1,
		PromptVersion:     "meal-decision-v10-fresh-exploration",
		Prompt:            "choose dinner",
		ResponseSchema:    json.RawMessage(`{"type":"object","properties":{"choice":{"type":"string"}},"required":["choice"],"additionalProperties":false}`),
		SemanticSignature: "sha256:abc",
	}
}

type fixedJobProvider struct{}

type failingJobProvider struct{}

type failingTokenReconciler struct{ calls int }

func (reconciler *failingTokenReconciler) Reconcile(context.Context, string, int, int, time.Time) error {
	reconciler.calls++
	return errors.New("redis unavailable")
}

func (failingJobProvider) Complete(context.Context, contracts.Request) (providerapi.Response, error) {
	return providerapi.Response{}, errors.New("upstream unavailable")
}

func (failingJobProvider) Stream(context.Context, contracts.Request, func(providerapi.StreamEvent) error) error {
	return errors.New("not used")
}

func (fixedJobProvider) Complete(context.Context, contracts.Request) (providerapi.Response, error) {
	return providerapi.Response{Content: `{"choice":"soup"}`, InputTokens: 5, OutputTokens: 3}, nil
}

func (fixedJobProvider) Stream(context.Context, contracts.Request, func(providerapi.StreamEvent) error) error {
	return errors.New("not used")
}
