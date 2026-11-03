package albums

import (
	"context"
	"errors"
	"time"
)

type DerivedFinalizationRepository interface {
	PendingDerivedFinalizations(context.Context, int) ([]DerivedVersionRef, error)
	FinalizeDerivedBudget(context.Context, DerivedVersionRef, time.Time) (DerivedFinalization, error)
}
type DerivedFinalizationBatch struct {
	Closed, Held, Deferred, Failed int
	ReleasedBytes                  int64
}
type DerivedFinalizationWorker struct {
	repository DerivedFinalizationRepository
	batchSize  int
	now        func() time.Time
}

func NewDerivedFinalizationWorker(repository DerivedFinalizationRepository, batchSize int, now func() time.Time) (*DerivedFinalizationWorker, error) {
	if repository == nil || batchSize < 1 || batchSize > 100 || now == nil {
		return nil, ErrInvalidManifest
	}
	return &DerivedFinalizationWorker{repository, batchSize, now}, nil
}
func (w *DerivedFinalizationWorker) RunOnce(ctx context.Context) (DerivedFinalizationBatch, error) {
	var b DerivedFinalizationBatch
	refs, err := w.repository.PendingDerivedFinalizations(ctx, w.batchSize)
	if err != nil {
		return b, err
	}
	if len(refs) > w.batchSize {
		return b, ErrConflict
	}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return b, err
		}
		f, err := w.repository.FinalizeDerivedBudget(ctx, ref, w.now().UTC())
		if errors.Is(err, ErrDerivedPending) {
			b.Deferred++
			continue
		}
		if err != nil {
			b.Failed++
			continue
		}
		b.Closed++
		if f.HeldBytes > 0 {
			b.Held++
		}
		b.ReleasedBytes += f.ReleasedBytes
	}
	return b, nil
}
func (w *DerivedFinalizationWorker) Run(ctx context.Context, interval time.Duration, observe func(DerivedFinalizationBatch, bool)) error {
	if interval < time.Second || interval > time.Minute {
		return ErrInvalidManifest
	}
	for {
		b, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if observe != nil {
			observe(b, err == nil)
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
