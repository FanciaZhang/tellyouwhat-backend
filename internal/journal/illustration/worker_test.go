package illustration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type workerStore struct {
	mu   sync.Mutex
	task Task
}

func (s *workerStore) GetTask(ctx context.Context, owner, id string, now time.Time) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.task, nil
}
func (s *workerStore) Claim(ctx context.Context, owner, id string, revision uint64, now time.Time) (Task, Input, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.task.Start(revision, now)
	return s.task, Input{Prompt: "confirmed"}, err
}
func (s *workerStore) AbortDispatch(ctx context.Context, claimed Task) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.task.AbortBeforeDispatch()
	return s.task, err
}
func (s *workerStore) StoreOutcome(ctx context.Context, claimed Task, result *Result, failure error, now time.Time) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.task, err
	}
	_, err := s.task.RecordOutcome(s.task.Revision, result, failure, now)
	return s.task, err
}

type guardFunc func(context.Context, Task) error

func (f guardFunc) Authorize(ctx context.Context, t Task) error { return f(ctx, t) }

type workerGenerator struct {
	calls atomic.Int32
	run   func(context.Context, Input) (Result, error)
}

func (g *workerGenerator) Model() string { return "image-model" }
func (g *workerGenerator) Generate(ctx context.Context, input Input) (Result, error) {
	g.calls.Add(1)
	return g.run(ctx, input)
}

func TestWorkerConcurrentDispatchAndFrozenModel(t *testing.T) {
	store := &workerStore{task: confirmedTask(t)}
	result, _ := inspectImage(testPNG(t))
	g := &workerGenerator{run: func(context.Context, Input) (Result, error) { return result, nil }}
	w := Worker{Store: store, Generator: g, Guard: guardFunc(func(_ context.Context, task Task) error {
		if task.ConsentRevision != "illustration-v1" {
			return ErrInput
		}
		return nil
	}), Now: func() time.Time { return time.Unix(1000, 0) }}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = w.Process(context.Background(), "owner", store.task.ID) }()
	}
	wg.Wait()
	if g.calls.Load() != 1 || store.task.State != TaskSucceeded {
		t.Fatal("duplicate dispatch or missing result")
	}
	store.task = confirmedTask(t)
	store.task.Model = "changed-model"
	if _, err := w.Process(context.Background(), "owner", store.task.ID); !errors.Is(err, ErrWorkerUnavailable) || g.calls.Load() != 1 {
		t.Fatal("model silently substituted")
	}
}

func TestWorkerAuthorizationWithdrawalBeforeDispatch(t *testing.T) {
	for _, denyAt := range []int{1, 2} {
		store := &workerStore{task: confirmedTask(t)}
		calls := 0
		g := &workerGenerator{run: func(context.Context, Input) (Result, error) { t.Fatal("unauthorized generation"); return Result{}, nil }}
		denied := errors.New("authorization revoked")
		w := Worker{Store: store, Generator: g, Guard: guardFunc(func(context.Context, Task) error {
			calls++
			if calls == denyAt {
				return denied
			}
			return nil
		}), Now: func() time.Time { return time.Unix(1000, 0) }}
		if _, err := w.Process(context.Background(), "owner", store.task.ID); !errors.Is(err, denied) {
			t.Fatal(err)
		}
		if g.calls.Load() != 0 {
			t.Fatal("unauthorized call")
		}
		if denyAt == 2 && (store.task.State != TaskCancelled || store.task.Charge != ChargeRelease) {
			t.Fatal("unused reservation retained")
		}
	}
}

func TestWorkerRecordsOutcomeAfterContextCancellation(t *testing.T) {
	store := &workerStore{task: confirmedTask(t)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &workerGenerator{run: func(context.Context, Input) (Result, error) { cancel(); return Result{}, ErrOutcomeUnknown }}
	w := Worker{Store: store, Generator: g, Guard: guardFunc(func(context.Context, Task) error { return nil }), Now: func() time.Time { return time.Unix(1000, 0) }}
	if _, err := w.Process(ctx, "owner", store.task.ID); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if store.task.State != TaskUncertain || store.task.Charge != ChargeUncertain {
		t.Fatal("cancelled context lost accounting outcome")
	}
	_, _ = w.Process(context.Background(), "owner", store.task.ID)
	if g.calls.Load() != 1 {
		t.Fatal("uncertain task regenerated")
	}
}

func TestWorkerCancellationDuringGenerationSuppressesResult(t *testing.T) {
	store := &workerStore{task: confirmedTask(t)}
	result, _ := inspectImage(testPNG(t))
	g := &workerGenerator{run: func(context.Context, Input) (Result, error) {
		store.mu.Lock()
		defer store.mu.Unlock()
		_ = store.task.Cancel(store.task.Revision)
		return result, nil
	}}
	w := Worker{Store: store, Generator: g, Guard: guardFunc(func(context.Context, Task) error { return nil }), Now: func() time.Time { return time.Unix(1000, 0) }}
	task, err := w.Process(context.Background(), "owner", store.task.ID)
	if err != nil || task.State != TaskCancelled || task.Charge != ChargeOneImage || task.ResultDigest != "" {
		t.Fatal("cancelled generation resurrected")
	}
	w.Guard = nil
	if _, err := w.Process(context.Background(), "owner", task.ID); !errors.Is(err, ErrWorkerUnavailable) {
		t.Fatal("missing authorization accepted")
	}
}
