package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

const liveMultipartCheckLease = 20 * time.Minute

// These are reconciliation jobs, not complete backups or erased accounts.
type LivePairMultipartBatch struct{ Resolved, Deferred, Contended int }
type LivePairMultipartWorker struct {
	service   *LivePairMultipartService
	batchSize int
}
type liveMultipartCheck struct {
	owner, pair, part, intent, state, token string
	attempts                                int
}

func NewLivePairMultipartWorker(repository *MySQLUploads, storage *COSObjects, batchSize int, now func() time.Time) (*LivePairMultipartWorker, error) {
	if batchSize < 1 || batchSize > 100 || storage == nil {
		return nil, ErrInvalidManifest
	}
	service, err := newLivePairMultipartService(repository, storage, now)
	if err != nil {
		return nil, err
	}
	return &LivePairMultipartWorker{service, batchSize}, nil
}

// Unknown writes are only observed. Abort is limited to a deletion accepted and
// fenced by the same request; a Complete already in flight is never aborted.
const liveMultipartCheckEligible = `(m.state IN ('initiating','unknown','completing','completion_unknown') OR (m.state IN ('ready','aborting') AND EXISTS (SELECT 1 FROM album_account_deletions d JOIN album_storage_accounts a ON a.owner_id=d.account_id WHERE d.account_id=p.owner_id AND a.deletion_requested_at=d.requested_at)))`

func (w *LivePairMultipartWorker) RunOnce(ctx context.Context) (LivePairMultipartBatch, error) {
	var result LivePairMultipartBatch
	r, now := w.service.repository, w.service.now
	at := now().UTC()
	if at.IsZero() {
		return result, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT p.owner_id,m.pair_upload_id,m.resource_id,m.intent_id FROM album_live_pair_multiparts m JOIN album_live_pairs p ON p.pair_upload_id=m.pair_upload_id WHERE p.direct_upload_issued=FALSE AND `+liveMultipartCheckEligible+` AND (m.check_after IS NULL OR m.check_after<=?) AND (m.check_until IS NULL OR m.check_until<=?) ORDER BY COALESCE(m.check_after,m.updated_at),m.pair_upload_id,m.resource_id LIMIT ?`, at, at, w.batchSize)
	if err != nil {
		return result, err
	}
	var jobs []liveMultipartCheck
	for rows.Next() {
		var job liveMultipartCheck
		if err = rows.Scan(&job.owner, &job.pair, &job.part, &job.intent); err != nil {
			rows.Close()
			return result, err
		}
		jobs = append(jobs, job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, candidate := range jobs {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		job, err := r.claimLiveMultipartCheck(ctx, candidate, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		call, cancel := context.WithTimeout(ctx, 15*time.Minute)
		var callErr error
		if job.state == "ready" || job.state == "aborting" {
			callErr = w.service.AbortForDeletion(call, job.owner, job.pair, job.part)
		} else {
			callErr = w.service.Reconcile(call, job.owner, job.pair, job.part)
		}
		cancel()
		// Provider receipts are saved by the service even during shutdown. Release
		// the lease separately; failed persistence leaves a bounded crash lease.
		finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		finishErr := r.finishLiveMultipartCheck(finish, job, callErr == nil, now().UTC())
		stop()
		if errors.Is(finishErr, ErrConflict) {
			result.Contended++
		} else if finishErr != nil {
			return result, finishErr
		} else if callErr != nil {
			result.Deferred++
		} else {
			result.Resolved++
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (r *MySQLUploads) claimLiveMultipartCheck(ctx context.Context, c liveMultipartCheck, at time.Time) (liveMultipartCheck, error) {
	if !validOwner(c.intent) || at.IsZero() {
		return c, ErrInvalidManifest
	}
	_, err := r.mutateLivePair(ctx, c.owner, c.pair, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		if err := noDirectLiveUpload(ctx, tx, u); err != nil {
			return u, err
		}
		m, err := lockLiveMultipart(ctx, tx, c.pair, c.part)
		if err != nil {
			return u, err
		}
		if m.IntentID != c.intent {
			return u, ErrConflict
		}
		var eligible bool
		if err = tx.QueryRowContext(ctx, `SELECT `+liveMultipartCheckEligible+` FROM album_live_pair_multiparts m JOIN album_live_pairs p ON p.pair_upload_id=m.pair_upload_id WHERE m.pair_upload_id=? AND m.resource_id=?`, c.pair, c.part).Scan(&eligible); err != nil {
			return u, err
		}
		if !eligible {
			return u, ErrConflict
		}
		c.token, c.state = uuid.NewString(), m.State
		result, err := tx.ExecContext(ctx, `UPDATE album_live_pair_multiparts SET check_token=?,check_until=?,check_attempts=LEAST(check_attempts+1,1000000) WHERE pair_upload_id=? AND resource_id=? AND (check_after IS NULL OR check_after<=?) AND (check_until IS NULL OR check_until<=?)`, c.token, at.Add(liveMultipartCheckLease), c.pair, c.part, at, at)
		if err != nil {
			return u, err
		}
		if n, e := result.RowsAffected(); e != nil || n != 1 {
			return u, ErrConflict
		}
		err = tx.QueryRowContext(ctx, `SELECT check_attempts FROM album_live_pair_multiparts WHERE pair_upload_id=? AND resource_id=?`, c.pair, c.part).Scan(&c.attempts)
		return u, err
	})
	if err != nil {
		return liveMultipartCheck{}, err
	}
	return c, nil
}
func (r *MySQLUploads) finishLiveMultipartCheck(ctx context.Context, c liveMultipartCheck, resolved bool, at time.Time) error {
	if !validOwner(c.token) || !validOwner(c.intent) || c.attempts < 1 || at.IsZero() {
		return ErrInvalidManifest
	}
	next := at
	if !resolved {
		next = at.Add(accountDeletionRetry(c.attempts))
	}
	_, err := r.mutateLivePair(ctx, c.owner, c.pair, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		result, err := tx.ExecContext(ctx, `UPDATE album_live_pair_multiparts SET check_after=?,check_token=NULL,check_until=NULL WHERE pair_upload_id=? AND resource_id=? AND intent_id=? AND check_token=? AND check_until>?`, next, c.pair, c.part, c.intent, c.token, at)
		if err != nil {
			return u, err
		}
		if n, e := result.RowsAffected(); e != nil || n != 1 {
			return u, ErrConflict
		}
		return u, nil
	})
	return err
}
func (w *LivePairMultipartWorker) Run(ctx context.Context, interval time.Duration, observe func(LivePairMultipartBatch, bool)) error {
	if interval < time.Second || interval > time.Minute {
		return ErrInvalidManifest
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
