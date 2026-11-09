package albums

import (
	"context"
	"errors"
	"time"
)

type LivePairBatch struct{ Verified, Failed, Contended int }
type LivePairWorker struct {
	repository LivePairRepository
	service    *UploadService
	batchSize  int
}

func NewLivePairWorker(repository LivePairRepository, service *UploadService, batchSize int) (*LivePairWorker, error) {
	if repository == nil || service == nil || batchSize < 1 || batchSize > 100 {
		return nil, errors.New("invalid album live pair worker configuration")
	}
	return &LivePairWorker{repository, service, batchSize}, nil
}
func (w *LivePairWorker) RunOnce(ctx context.Context) (LivePairBatch, error) {
	var batch LivePairBatch
	jobs, err := w.repository.PendingLivePairs(ctx, w.service.now().UTC(), w.batchSize)
	if err != nil {
		return batch, err
	}
	if len(jobs) > w.batchSize {
		return batch, ErrConflict
	}
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return batch, err
		}
		_, err := w.service.VerifyLivePair(ctx, job.OwnerID, job.ID)
		if ctx.Err() != nil {
			return batch, ctx.Err()
		}
		switch {
		case err == nil:
			batch.Verified++
		case errors.Is(err, ErrLease):
			batch.Contended++
		default:
			batch.Failed++
		}
	}
	return batch, nil
}
func (w *LivePairWorker) Run(ctx context.Context, interval time.Duration, observe func(LivePairBatch, bool)) error {
	if interval < time.Second || interval > time.Minute {
		return ErrInvalidManifest
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
