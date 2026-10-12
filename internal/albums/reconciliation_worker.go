package albums

import (
	"context"
	"errors"
	"time"
)

type ReconciliationQueue interface {
	PendingReconciliations(context.Context, time.Time, int) ([]ReconciliationJob, error)
	ClaimReconciliation(context.Context, ReconciliationJob, time.Time) (ReconciliationJob, error)
	FinishReconciliation(context.Context, ReconciliationJob, time.Time) error
}

type ReconciliationBatch struct{ Initialized, Assembled, Failed, Contended int }

type ReconciliationWorker struct {
	queue     ReconciliationQueue
	reconcile func(context.Context, ReconciliationJob) error
	now       func() time.Time
	batchSize int
}

func NewReconciliationWorker(queue ReconciliationQueue, service *MultipartService, batchSize int) (*ReconciliationWorker, error) {
	if queue == nil || service == nil || batchSize < 1 || batchSize > 100 {
		return nil, errors.New("invalid reconciliation worker configuration")
	}
	return &ReconciliationWorker{queue: queue, now: service.now, batchSize: batchSize, reconcile: func(ctx context.Context, job ReconciliationJob) error {
		switch job.Kind {
		case "initialization":
			_, err := service.reconcileInitialization(ctx, job.OwnerID, job.UploadID, job.ResourceID, job.OperationID)
			return err
		case "completion":
			_, err := service.reconcileCompletion(ctx, job.OwnerID, job.UploadID, job.ResourceID, job.OperationID)
			return err
		default:
			return ErrInvalidManifest
		}
	}}, nil
}
func (w *ReconciliationWorker) RunOnce(ctx context.Context) (ReconciliationBatch, error) {
	var result ReconciliationBatch
	if err := ctx.Err(); err != nil {
		return result, err
	}
	jobs, err := w.queue.PendingReconciliations(ctx, w.now().UTC(), w.batchSize)
	if err != nil {
		return result, err
	}
	if len(jobs) > w.batchSize {
		return result, errors.New("reconciliation queue exceeded batch size")
	}
	for _, candidate := range jobs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		job, err := w.queue.ClaimReconciliation(ctx, candidate, w.now().UTC())
		if err != nil {
			if errors.Is(err, ErrLease) || errors.Is(err, ErrNotFound) {
				result.Contended++
			} else {
				result.Failed++
			}
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		readErr := w.reconcile(readCtx, job)
		cancel()
		// Release/backoff even when shutdown interrupted the read. A crash leaves the
		// bounded lease and already consumed attempt for the next process.
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		finishErr := w.queue.FinishReconciliation(finishCtx, job, w.now().UTC())
		finishCancel()
		if readErr == nil && finishErr == nil {
			if job.Kind == "initialization" {
				result.Initialized++
			} else {
				result.Assembled++
			}
		} else {
			result.Failed++
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}
	return result, nil
}

var _ ReconciliationQueue = (*MySQLUploads)(nil)

func (w *ReconciliationWorker) Run(ctx context.Context, interval time.Duration, observe func(ReconciliationBatch, bool)) error {
	if interval < time.Second || interval > time.Minute {
		return errors.New("invalid reconciliation poll interval")
	}
	for {
		batch, err := w.RunOnce(ctx)
		if observe != nil {
			observe(batch, err == nil)
		}
		if ctx.Err() != nil {
			return ctx.Err()
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
