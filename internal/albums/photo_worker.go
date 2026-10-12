package albums

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type PhotoJobQueue interface {
	PendingPhotoJobs(context.Context, time.Time, int) ([]PhotoJob, error)
	ClaimPhotoJob(context.Context, string, string, string, time.Time) (PhotoJob, error)
	FinishPhotoJob(context.Context, PhotoJob, bool, bool, time.Time) error
}
type PhotoJobBatch struct{ Stored, Failed, Contended int }
type PhotoWorker struct {
	queue     PhotoJobQueue
	service   *UploadService
	batchSize int
}

func NewPhotoWorker(queue PhotoJobQueue, service *UploadService, batchSize int) (*PhotoWorker, error) {
	if queue == nil || service == nil || batchSize < 1 || batchSize > 100 {
		return nil, errors.New("invalid photo worker configuration")
	}
	return &PhotoWorker{queue, service, batchSize}, nil
}
func (w *PhotoWorker) RunOnce(ctx context.Context) (PhotoJobBatch, error) {
	var batch PhotoJobBatch
	if err := ctx.Err(); err != nil {
		return batch, err
	}
	jobs, err := w.queue.PendingPhotoJobs(ctx, w.service.now().UTC(), w.batchSize)
	if err != nil {
		return batch, err
	}
	if len(jobs) > w.batchSize {
		return batch, ErrConflict
	}
	for _, pending := range jobs {
		if err := ctx.Err(); err != nil {
			return batch, err
		}
		job, err := w.queue.ClaimPhotoJob(ctx, pending.OwnerID, pending.ID, uuid.NewString(), w.service.now().UTC())
		if errors.Is(err, ErrLease) {
			batch.Contended++
			continue
		}
		if err != nil {
			return batch, err
		}
		// Bound the entire operation below the ten-minute lease. No goroutine keeps
		// running a timed-out render/upload, and generation is serial per process.
		operation, cancel := context.WithTimeout(ctx, 6*time.Minute)
		_, err = w.service.GeneratePhotoDerivative(operation, job.OwnerID, job.VersionID, job.Preset)
		cancel()
		if ctx.Err() != nil {
			return batch, ctx.Err()
		}
		// A concurrent trash may hide the version; restoration can resume this job.
		terminal := errors.Is(err, ErrQuota) || errors.Is(err, ErrInvalidManifest) || errors.Is(err, ErrIncompleteBackup)
		finishErr := w.queue.FinishPhotoJob(ctx, job, err == nil, terminal, w.service.now().UTC())
		if errors.Is(finishErr, ErrLease) {
			batch.Contended++
			continue
		}
		if finishErr != nil {
			return batch, finishErr
		}
		if err == nil {
			batch.Stored++
		} else {
			batch.Failed++
		}
	}
	return batch, nil
}
func (w *PhotoWorker) Run(ctx context.Context, interval time.Duration, observe func(PhotoJobBatch, bool)) error {
	if interval < time.Second || interval > time.Minute {
		return errors.New("invalid photo poll interval")
	}
	for {
		batch, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if observe != nil {
			observe(batch, err == nil)
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
