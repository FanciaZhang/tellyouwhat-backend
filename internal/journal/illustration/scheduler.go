package illustration

import (
	"context"
	"errors"
	"time"
)

type QueueRepository interface {
	Pending(context.Context, time.Time, int) ([]Task, error)
	Expire(context.Context, time.Time, int) error
	Cancel(context.Context, string, string, uint64) (Task, error)
}
type TaskProcessor interface {
	Process(context.Context, string, string) (Task, error)
}

// Scheduler is owned by the application lifecycle. It runs bounded batches
// sequentially; the durable worker claim arbitrates concurrent server instances.
type Scheduler struct {
	Queue     QueueRepository
	Processor TaskProcessor
	Now       func() time.Time
}

func (s Scheduler) Tick(ctx context.Context) error {
	if s.Queue == nil || s.Processor == nil {
		return ErrWorkerUnavailable
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	if err := s.Queue.Expire(ctx, now, 32); err != nil {
		return err
	}
	tasks, err := s.Queue.Pending(ctx, now, 8)
	if err != nil {
		return err
	}
	var failures []error
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return err
		}
		dispatch, cancel := context.WithTimeout(ctx, 4*time.Minute)
		_, err := s.Processor.Process(dispatch, task.OwnerID, task.ID)
		cancel()
		if errors.Is(err, ErrAccessDenied) {
			// A definitively revoked queued task must not block later queue entries.
			// Revision fencing prevents cancelling a concurrently dispatched task.
			_, err = s.Queue.Cancel(ctx, task.OwnerID, task.ID, task.Revision)
		}
		if err != nil && !errors.Is(err, ErrTaskConflict) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Run reports errors to the host without logging private task/provider data.
// Cancellation waits for the active processor's bounded outcome persistence.
func (s Scheduler) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if interval <= 0 || s.Queue == nil || s.Processor == nil {
		return ErrWorkerUnavailable
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.Tick(ctx); err != nil && report != nil {
			report(err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
