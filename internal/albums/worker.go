package albums

import (
	"context"
	"errors"
	"time"
)

// PendingUploads supplies only runnable durable jobs. Claim in Verify remains
// authoritative, so several worker processes may safely observe the same page.
type PendingUploads interface {
	Pending(context.Context, time.Time, int) ([]Upload, error)
}

type VerificationBatch struct {
	Verified        int
	Failed          int
	Contended       int
	CleanupRequired int
}

// VerificationWorker uses the database queue, not a lossy in-memory notification.
// A cancelled or failed verification keeps its lease until expiry; the next
// process can resume it without accepting a client receipt as proof.
type VerificationWorker struct {
	pending   PendingUploads
	service   *UploadService
	batchSize int
	interval  time.Duration
}

func NewVerificationWorker(pending PendingUploads, service *UploadService, batchSize int, interval time.Duration) (*VerificationWorker, error) {
	if pending == nil || service == nil || batchSize < 1 || batchSize > 100 || interval < time.Second || interval > time.Minute {
		return nil, errors.New("invalid album verification worker configuration")
	}
	return &VerificationWorker{pending, service, batchSize, interval}, nil
}

func (w *VerificationWorker) RunOnce(ctx context.Context) (VerificationBatch, error) {
	var result VerificationBatch
	if err := ctx.Err(); err != nil {
		return result, err
	}
	jobs, err := w.pending.Pending(ctx, w.service.now().UTC(), w.batchSize)
	if err != nil {
		return result, err
	}
	// Do not let a broken queue implementation cause an unbounded work batch.
	if len(jobs) > w.batchSize {
		return result, errors.New("album queue exceeded requested batch size")
	}
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		_, err := w.service.Verify(ctx, job.OwnerID, job.ID)
		if errors.Is(err, ErrCOSCleanup) {
			result.CleanupRequired++
		}
		switch {
		case err == nil:
			result.Verified++
		case ctx.Err() != nil:
			if errors.Is(err, ErrCOSCleanup) {
				result.Failed++
			}
			return result, ctx.Err()
		case errors.Is(err, ErrLease), errors.Is(err, ErrExpired):
			result.Contended++
		default:
			// One unavailable object must not prevent other accounts from progressing.
			// Do not expose provider errors, signed URLs or asset IDs to an observer.
			result.Failed++
		}
	}
	return result, nil
}

// Run polls immediately and then at a bounded interval. observe receives only
// aggregate counters and queue availability; raw provider errors stay private.
// The caller owns process lifecycle and must wait for Run before closing the DB.
func (w *VerificationWorker) Run(ctx context.Context, observe func(VerificationBatch, bool)) error {
	for {
		result, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			if observe != nil && result.CleanupRequired > 0 {
				observe(result, false)
			}
			return ctx.Err()
		}
		if observe != nil {
			observe(result, err == nil)
		}
		timer := time.NewTimer(w.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
