package illustration

import (
	"context"
	"errors"
	"testing"
	"time"
)

type queueFixture struct {
	tasks     []Task
	expired   bool
	cancelled []string
}

func (q *queueFixture) Pending(context.Context, time.Time, int) ([]Task, error) {
	if !q.expired {
		return nil, ErrTaskConflict
	}
	return q.tasks, nil
}
func (q *queueFixture) Expire(context.Context, time.Time, int) error { q.expired = true; return nil }
func (q *queueFixture) Cancel(_ context.Context, _ string, id string, _ uint64) (Task, error) {
	q.cancelled = append(q.cancelled, id)
	return Task{}, nil
}

type processFunc func(context.Context, string, string) (Task, error)

func (f processFunc) Process(ctx context.Context, owner, id string) (Task, error) {
	return f(ctx, owner, id)
}

func TestSchedulerContinuesBatchAndCancelsOnlyRevoked(t *testing.T) {
	q := &queueFixture{tasks: []Task{{ID: "revoked"}, {ID: "conflict"}, {ID: "unknown"}, {ID: "ok"}}}
	var calls []string
	s := Scheduler{Queue: q, Processor: processFunc(func(ctx context.Context, owner, id string) (Task, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded generation")
		}
		calls = append(calls, id)
		switch id {
		case "revoked":
			return Task{}, ErrAccessDenied
		case "conflict":
			return Task{}, ErrTaskConflict
		case "unknown":
			return Task{}, ErrOutcomeUnknown
		}
		return Task{}, nil
	})}
	if err := s.Tick(context.Background()); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if len(calls) != 4 || len(q.cancelled) != 1 || q.cancelled[0] != "revoked" {
		t.Fatalf("calls=%v cancel=%v", calls, q.cancelled)
	}
}

func TestSchedulerShutdownDoesNotStartNextTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &queueFixture{tasks: []Task{{ID: "one"}, {ID: "two"}}}
	calls := 0
	s := Scheduler{Queue: q, Processor: processFunc(func(context.Context, string, string) (Task, error) { calls++; cancel(); return Task{}, nil })}
	if err := s.Run(ctx, time.Hour, nil); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if err := (Scheduler{}).Run(context.Background(), time.Second, nil); !errors.Is(err, ErrWorkerUnavailable) {
		t.Fatal(err)
	}
}
