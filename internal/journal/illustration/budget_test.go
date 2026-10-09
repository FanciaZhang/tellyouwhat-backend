package illustration

import (
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"sync/atomic"
	"testing"
	"time"
)

func imageBudgetController(t *testing.T, store costcontrol.Store, limit int64) *costcontrol.Controller {
	t.Helper()
	c, err := costcontrol.New(store, costcontrol.Limits{MonthlyBudgetNanos: limit, MaxConcurrent: 1, LeaseDuration: time.Minute}, func() time.Time { return time.Unix(1000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestImageBudgetPreservesCostForSuccessAndUnknown(t *testing.T) {
	for _, failure := range []error{nil, ErrOutcomeUnknown, Rejected{Status: 429}} {
		c := imageBudgetController(t, costcontrol.NewMemoryStore(), 15)
		ctx, cancel := context.WithCancel(context.Background())
		next := &workerGenerator{run: func(context.Context, Input) (Result, error) {
			cancel()
			return Result{Image: []byte("result")}, failure
		}}
		g := &BudgetedGenerator{Next: next, Controller: c, MaxCostNanos: 10}
		result, err := g.Generate(ctx, Input{Prompt: "scene"})
		if !errors.Is(err, failure) || len(result.Image) == 0 || next.calls.Load() != 1 {
			t.Fatalf("result lost: %v", err)
		}
		// Same shared ceiling, including requests from Health; no isolated budget.
		if _, err := c.Reserve(context.Background(), "health", "text", "ark", 6); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
			t.Fatalf("image charge forgiven or concurrency leaked: %v", err)
		}
		lease, err := c.Reserve(context.Background(), "health", "text", "ark", 5)
		if err != nil {
			t.Fatal("settlement did not release concurrency", err)
		}
		if err := lease.Settle(context.Background(), 5, true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImageBudgetDenialDoesNotInvokeProvider(t *testing.T) {
	next := &workerGenerator{run: func(context.Context, Input) (Result, error) {
		t.Fatal("unbudgeted provider invocation")
		return Result{}, nil
	}}
	g := &BudgetedGenerator{Next: next, Controller: imageBudgetController(t, costcontrol.NewMemoryStore(), 5), MaxCostNanos: 10}
	_, err := g.Generate(context.Background(), Input{Prompt: "scene"})
	var undispatched NotDispatched
	if !errors.As(err, &undispatched) || !errors.Is(err, costcontrol.ErrBudgetExceeded) || next.calls.Load() != 0 {
		t.Fatalf("unsafe denial: %v", err)
	}
}

type workerGenerator struct {
	calls atomic.Int32
	run   func(context.Context, Input) (Result, error)
}

func (g *workerGenerator) Model() string { return "image-model" }
func (g *workerGenerator) Generate(ctx context.Context, input Input) (Result, error) {
	g.calls.Add(1)
	return g.run(ctx, input)
}

type failingSettlementStore struct{ costcontrol.Store }

func (s failingSettlementStore) Settle(context.Context, string, int64, bool, time.Time) error {
	return errors.New("storage offline")
}
func TestImageBudgetSettlementFailureKeepsSuccessfulImage(t *testing.T) {
	base := costcontrol.NewMemoryStore()
	c := imageBudgetController(t, failingSettlementStore{base}, 10)
	reported := 0
	g := &BudgetedGenerator{Next: &workerGenerator{run: func(context.Context, Input) (Result, error) { return Result{Image: []byte("image")}, nil }}, Controller: c, MaxCostNanos: 10, SettlementFailed: func() { reported++ }}
	result, err := g.Generate(context.Background(), Input{})
	if err != nil || len(result.Image) == 0 || reported != 1 {
		t.Fatal("settlement failure discarded generated image")
	}
	if _, err := c.Reserve(context.Background(), "journal", "image", "ark_image", 1); err == nil {
		t.Fatal("failed settlement released reservation")
	}
}
