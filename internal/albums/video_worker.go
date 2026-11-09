package albums

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type VideoJobQueue interface {
	PendingVideoJobs(context.Context, time.Time, int) ([]VideoJob, error)
	ClaimVideoJob(context.Context, string, string, string, time.Time) (VideoJob, error)
	FinishVideoStep(context.Context, VideoJob, bool, bool, time.Time) error
}
type VideoJobBatch struct{ Stored, Waiting, Failed, Contended int }
type VideoWorker struct {
	queue     VideoJobQueue
	service   *UploadService
	queueID   string
	batchSize int
}

func NewVideoWorker(queue VideoJobQueue, service *UploadService, queueID string, batchSize int) (*VideoWorker, error) {
	if queue == nil || service == nil || !ciQueuePattern.MatchString(queueID) || batchSize < 1 || batchSize > 100 {
		return nil, errors.New("invalid video worker configuration")
	}
	return &VideoWorker{queue, service, queueID, batchSize}, nil
}

func (w *VideoWorker) RunOnce(ctx context.Context) (VideoJobBatch, error) {
	var batch VideoJobBatch
	jobs, err := w.queue.PendingVideoJobs(ctx, w.service.now().UTC(), w.batchSize)
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
		job, err := w.queue.ClaimVideoJob(ctx, pending.OwnerID, pending.ID, uuid.NewString(), w.service.now().UTC())
		if errors.Is(err, ErrLease) {
			batch.Contended++
			continue
		}
		if err != nil {
			return batch, err
		}
		operation, cancel := context.WithTimeout(ctx, 6*time.Minute)
		complete, stepErr := w.service.AdvanceVideoJob(operation, job, w.queueID)
		cancel()
		if ctx.Err() != nil {
			return batch, ctx.Err()
		}
		if complete {
			batch.Stored++
			continue
		}
		// Refresh durable submission state so a successful first POST waits one
		// minute, and an unknown outcome never repeats submission on the next lease.
		repository, ok := w.service.repository.(VideoJobRepository)
		if !ok {
			return batch, ErrConflict
		}
		current, readErr := repository.VideoJob(ctx, job.OwnerID, job.VersionID, job.Preset)
		if readErr != nil {
			return batch, readErr
		}
		if !videoLease(current, job, w.service.now().UTC()) {
			batch.Contended++
			continue
		}
		// A concurrent trash may hide the version; restoration can resume this job.
		terminal := errors.Is(stepErr, ErrVideoFailed) || errors.Is(stepErr, ErrVideoUnsupported) || errors.Is(stepErr, ErrInvalidManifest) ||
			errors.Is(stepErr, ErrIncompleteBackup) || errors.Is(stepErr, ErrQuota) || errors.Is(stepErr, ErrConflict)
		finishErr := w.queue.FinishVideoStep(ctx, current, stepErr != nil, terminal, w.service.now().UTC())
		if errors.Is(finishErr, ErrLease) {
			batch.Contended++
			continue
		}
		if finishErr != nil {
			return batch, finishErr
		}
		if stepErr != nil {
			batch.Failed++
		} else {
			batch.Waiting++
		}
	}
	return batch, nil
}

func (w *VideoWorker) Run(ctx context.Context, interval time.Duration, observe func(VideoJobBatch, bool)) error {
	if interval < time.Second || interval > time.Minute {
		return errors.New("invalid video poll interval")
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
