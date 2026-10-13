package albums

import (
	"context"
	"time"
)

// Counts media convergence jobs, not files erased or accounts completely deleted.
type AccountMediaObservationBatch struct{ Acknowledged, Deferred, Contended int }

type AccountMediaObservationWorker struct {
	repository *MySQLUploads
	storage    *COSObjects
	batchSize  int
	now        func() time.Time
}

func NewAccountMediaObservationWorker(repository *MySQLUploads, storage *COSObjects, batchSize int, now func() time.Time) (*AccountMediaObservationWorker, error) {
	if repository == nil || storage == nil || batchSize < 1 || batchSize > 100 || now == nil {
		return nil, ErrInvalidManifest
	}
	return &AccountMediaObservationWorker{repository, storage, batchSize, now}, nil
}

func (w *AccountMediaObservationWorker) Check(ctx context.Context) error {
	for _, query := range []string{
		`SELECT d.account_id,d.request_id,d.requested_at,s.deletion_requested_at FROM album_account_deletions d JOIN album_storage_accounts s ON s.owner_id=d.account_id LIMIT 0`,
		`SELECT deletion_check_after,deletion_check_attempts,deletion_check_token,deletion_check_until,deletion_abort_id,deletion_aborted_at FROM album_multipart_intents LIMIT 0`,
		`SELECT object_version,legacy,deletion_check_after,deletion_check_attempts,deletion_check_token,deletion_check_until FROM album_original_seals LIMIT 0`,
		`SELECT deletion_object_version,deletion_check_after,deletion_check_attempts,deletion_check_token,deletion_check_until FROM album_photo_writes LIMIT 0`,
		`SELECT account_id,request_id,job_id,acknowledged_at,available_at,lease_until,lease_token,attempts,provider_job_id,provider_succeeded FROM album_deletion_video_jobs LIMIT 0`,
		`SELECT photo_copy_version,movie_copy_version,deletion_check_after,deletion_check_attempts,deletion_check_token,deletion_check_until FROM album_live_pairs LIMIT 0`,
	} {
		rows, err := w.repository.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Each queue persists its own lease and backoff. Provider errors defer that job;
// database errors stop this pass without logging credentials or media paths.
func (w *AccountMediaObservationWorker) RunOnce(ctx context.Context) (AccountMediaObservationBatch, error) {
	var b AccountMediaObservationBatch
	originals, err := w.repository.runOriginalDeletionBatch(ctx, w.storage.ReconcileSeal, w.batchSize, w.now)
	b.Acknowledged += originals.Acknowledged
	b.Deferred += originals.Deferred
	b.Contended += originals.Contended
	if err != nil {
		return b, err
	}
	photos, err := w.repository.runPhotoDeletionBatch(ctx, w.storage.observePhotoDeletion, w.batchSize, w.now)
	b.Acknowledged += photos.Acknowledged
	b.Deferred += photos.Deferred
	b.Contended += photos.Contended
	if err != nil {
		return b, err
	}
	videos, err := w.repository.runVideoDeletionBatch(ctx, w.storage.InspectVideoCleanup, w.batchSize, w.now)
	b.Acknowledged += videos.Acknowledged
	b.Deferred += videos.Deferred
	b.Contended += videos.Contended
	if err != nil {
		return b, err
	}
	pairs, err := w.repository.runLivePairDeletionBatch(ctx, w.storage.observeLivePairCopy, w.batchSize, w.now)
	b.Acknowledged += pairs.Acknowledged
	b.Deferred += pairs.Deferred
	b.Contended += pairs.Contended
	if err != nil {
		return b, err
	}
	multipart, err := w.repository.runMultipartDeletionBatch(ctx, w.storage, w.batchSize, w.now)
	b.Acknowledged += multipart.Acknowledged
	b.Deferred += multipart.Deferred
	b.Contended += multipart.Contended
	return b, err
}

func (w *AccountMediaObservationWorker) Run(ctx context.Context, interval time.Duration, observe func(AccountMediaObservationBatch, bool)) error {
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
