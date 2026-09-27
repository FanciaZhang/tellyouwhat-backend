package illustration

import (
	"context"
	"errors"
	"time"
)

var ErrWorkerUnavailable = errors.New("illustration worker dependencies unavailable")

type TaskRepository interface {
	GetTask(context.Context, string, string, time.Time) (Task, error)
	Claim(context.Context, string, string, uint64, time.Time) (Task, Input, error)
	AbortDispatch(context.Context, Task) (Task, error)
	StoreOutcome(context.Context, Task, *Result, error, time.Time) (Task, error)
}

// DispatchGuard must verify current account entitlement, consent revision,
// material scope and an existing durable image reservation. No permissive
// production default is provided.
type DispatchGuard interface {
	Authorize(context.Context, Task) error
}
type ImageGenerator interface {
	Model() string
	Generate(context.Context, Input) (Result, error)
}

type Worker struct {
	Store     TaskRepository
	Guard     DispatchGuard
	Generator ImageGenerator
	Now       func() time.Time
}

func (w Worker) Process(ctx context.Context, owner, id string) (Task, error) {
	if w.Store == nil || w.Guard == nil || w.Generator == nil {
		return Task{}, ErrWorkerUnavailable
	}
	now := w.Now
	if now == nil {
		now = time.Now
	}
	task, err := w.Store.GetTask(ctx, owner, id, now())
	if err != nil {
		return Task{}, err
	}
	if task.State != TaskQueued {
		return task, ErrTaskConflict
	}
	if task.Model != w.Generator.Model() {
		return task, ErrWorkerUnavailable
	}
	if err := w.Guard.Authorize(ctx, task); err != nil {
		return task, err
	}
	claimed, input, err := w.Store.Claim(ctx, owner, id, task.Revision, now())
	if err != nil {
		return Task{}, err
	}
	// A separate bounded context allows outcome recording when the original
	// request is cancelled. It does not authorize another generation attempt.
	abort := func(cause error) (Task, error) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		stopped, err := w.Store.AbortDispatch(cleanup, claimed)
		if err != nil {
			return stopped, err
		}
		return stopped, cause
	}
	if err := ctx.Err(); err != nil {
		return abort(err)
	}
	current, err := w.Store.GetTask(ctx, owner, id, now())
	if err != nil {
		return abort(err)
	}
	if current.State != TaskRunning || current.Revision != claimed.Revision {
		return abort(ErrTaskConflict)
	}
	if err := w.Guard.Authorize(ctx, current); err != nil {
		return abort(err)
	}
	if err := ctx.Err(); err != nil {
		return abort(err)
	}
	result, failure := w.Generator.Generate(ctx, input)
	var notDispatched NotDispatched
	if errors.As(failure, &notDispatched) {
		return abort(failure)
	}
	var output *Result
	if failure == nil {
		output = &result
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	completed, err := w.Store.StoreOutcome(cleanup, claimed, output, failure, now())
	if err != nil {
		return completed, err
	}
	return completed, failure
}
