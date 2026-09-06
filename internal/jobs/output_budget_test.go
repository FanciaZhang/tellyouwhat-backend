package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
	"github.com/tellyouwhat/backend/internal/quota"
)

type budgetObservingProvider struct {
	fixedJobProvider
	budgets []contracts.OutputBudget
}

func (model *budgetObservingProvider) Complete(ctx context.Context, request contracts.Request) (providerapi.Response, error) {
	model.budgets = append(model.budgets, request.OutputBudget)
	if len(model.budgets) == 1 {
		return failingJobProvider{}.Complete(ctx, request)
	}
	return fixedJobProvider{}.Complete(ctx, request)
}

func TestJobOutputBudgetSurvivesIdempotentEnqueueAndRetry(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	service := NewService(store, time.Now)
	principal := attestation.Principal{KeyID: "key", DeviceID: "device", TransactionID: "transaction"}
	request := jobRequest()
	request.OutputBudget = contracts.OutputBudget{Version: "health-output-v1", MaxTokens: 16_384}
	job, err := service.Enqueue(ctx, principal, request, "body")
	if err != nil {
		t.Fatal(err)
	}
	replacement := request
	replacement.OutputBudget = contracts.DefaultOutputBudget()
	replay, err := service.Enqueue(ctx, principal, replacement, "body")
	if err != nil || replay.Request.OutputBudget != request.OutputBudget {
		t.Fatalf("enqueue replay changed budget: %+v err=%v", replay.Request.OutputBudget, err)
	}
	limiter := quota.NewMemoryLimiter(quota.Limits{DailyTokensPerTransaction: 100_000, MonthlyTokensPerTransaction: 1_000_000})
	prepayJob(t, limiter, job)
	model := &budgetObservingProvider{}
	worker := NewWorker(store, model, limiter)
	if err := worker.Process(ctx, job.ID); err == nil {
		t.Fatal("expected initial provider failure")
	}
	if err := worker.Process(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if len(model.budgets) != 2 || model.budgets[0] != request.OutputBudget || model.budgets[1] != request.OutputBudget {
		t.Fatalf("worker changed admitted budget across attempts: %+v", model.budgets)
	}
	want := contracts.ReservationTokens(request) + 8
	snapshot, err := limiter.Snapshot(ctx, principal.TransactionID, time.Now())
	if err != nil || snapshot.DailyUsed != want || snapshot.MonthlyUsed != want {
		t.Fatalf("quota did not use the frozen output budget: %+v want=%d err=%v", snapshot, want, err)
	}
}

func TestWorkerRefusesToInventLegacyJobOutputBudget(t *testing.T) {
	store, job, limiter := newBudgetedJob(t, 100_000, 1_000_000)
	store.mu.Lock()
	legacy := store.jobs[job.ID]
	legacy.Request.OutputBudget = contracts.OutputBudget{}
	store.jobs[job.ID] = legacy
	store.mu.Unlock()
	model := &budgetObservingProvider{}
	if err := NewWorker(store, model, limiter).Process(context.Background(), job.ID); err == nil || len(model.budgets) != 0 {
		t.Fatalf("unbudgeted legacy job called provider: calls=%d err=%v", len(model.budgets), err)
	}
	stored, err := store.Get(context.Background(), job.ID)
	if err != nil || stored.Status != StatusFailed || stored.FailureCategory != "execution_policy" {
		t.Fatalf("legacy job did not reach explicit failure: status=%s category=%s err=%v", stored.Status, stored.FailureCategory, err)
	}
}
