package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Media only: identity, billing and backup erasure have separate completion rules.
type AccountMediaErasureBatch struct{ Advanced, Settled, Deferred, Contended int }
type AccountMediaErasureWorker struct {
	repository *MySQLUploads
	storage    accountPurgeStorage
	batchSize  int
	now        func() time.Time
}

func NewAccountMediaErasureWorker(repository *MySQLUploads, storage *COSObjects, batchSize int, now func() time.Time) (*AccountMediaErasureWorker, error) {
	if repository == nil || storage == nil || batchSize < 1 || batchSize > 100 || now == nil {
		return nil, ErrInvalidManifest
	}
	return &AccountMediaErasureWorker{repository, storage, batchSize, now}, nil
}

func (w *AccountMediaErasureWorker) Check(ctx context.Context) error {
	rows, err := w.repository.db.QueryContext(ctx, `SELECT d.media_purge_after,d.media_quiesced_at,p.media_settled_at,p.plan_json,p.action_index,p.lease_token,p.lease_until FROM album_account_deletions d LEFT JOIN album_account_object_purges p ON p.account_id=d.account_id LIMIT 0`)
	if err != nil {
		return err
	}
	return rows.Close()
}

// A durable scheduling deadline covers both object deletion and metadata
// settlement. A crashed pass can be reclaimed; object actions retain their own
// exact-plan lease. Finishing an old pass cannot reschedule a newer claim.
func (w *AccountMediaErasureWorker) claim(ctx context.Context, owner, request string) (time.Time, error) {
	at := w.now().UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return time.Time{}, ErrInvalidManifest
	}
	tx, err := w.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	if err = lockAccountPurgeScope(ctx, tx, owner, request, at); err != nil {
		return time.Time{}, err
	}
	var after, settled sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT media_purge_after FROM album_account_deletions WHERE account_id=? AND request_id=? FOR UPDATE`, owner, request).Scan(&after); err != nil {
		return time.Time{}, err
	}
	if after.Valid && after.Time.After(at) {
		return time.Time{}, ErrLease
	}
	err = tx.QueryRowContext(ctx, `SELECT media_settled_at FROM album_account_object_purges WHERE account_id=? FOR UPDATE`, owner).Scan(&settled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	if settled.Valid {
		return time.Time{}, ErrLease
	}
	until := at.Add(accountPurgeLease)
	if _, err = tx.ExecContext(ctx, `UPDATE album_account_deletions SET media_purge_after=? WHERE account_id=? AND request_id=?`, until, owner, request); err != nil {
		return time.Time{}, err
	}
	return until, tx.Commit()
}

func (w *AccountMediaErasureWorker) finish(ctx context.Context, owner, request string, claim time.Time, failed bool) error {
	next := w.now().UTC().Truncate(time.Microsecond)
	if failed {
		next = next.Add(time.Minute)
	}
	_, err := w.repository.db.ExecContext(ctx, `UPDATE album_account_deletions SET media_purge_after=? WHERE account_id=? AND request_id=? AND media_purge_after=?`, next, owner, request, claim)
	return err
}

func (w *AccountMediaErasureWorker) RunOnce(ctx context.Context) (AccountMediaErasureBatch, error) {
	var b AccountMediaErasureBatch
	rows, err := w.repository.db.QueryContext(ctx, `SELECT d.account_id,d.request_id FROM album_account_deletions d
 JOIN album_accounts a ON a.account_id=d.account_id AND a.state='deleting'
 LEFT JOIN album_account_object_purges p ON p.account_id=d.account_id
 WHERE d.media_quiesced_at IS NOT NULL AND p.media_settled_at IS NULL
 AND (d.media_purge_after IS NULL OR d.media_purge_after<=?)
 AND (p.lease_until IS NULL OR p.lease_until<=?)
 ORDER BY COALESCE(d.media_purge_after,d.requested_at),d.account_id LIMIT ?`, w.now().UTC(), w.now().UTC(), w.batchSize)
	if err != nil {
		return b, err
	}
	var refs [][2]string
	for rows.Next() {
		var ref [2]string
		if err = rows.Scan(&ref[0], &ref[1]); err != nil {
			rows.Close()
			return b, err
		}
		refs = append(refs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return b, err
	}
	for _, ref := range refs {
		if err = ctx.Err(); err != nil {
			return b, err
		}
		until, e := w.claim(ctx, ref[0], ref[1])
		if errors.Is(e, ErrLease) {
			b.Contended++
			continue
		}
		if e != nil {
			return b, e
		}
		e = w.repository.runAccountObjectPurgeStep(ctx, ref[0], ref[1], w.storage, w.now)
		settled := false
		if e == nil {
			var p accountObjectPurge
			p, e = w.repository.accountObjectPurge(ctx, ref[0])
			if e == nil && p.State == "objects_removed" {
				_, e = w.repository.settleAccountMediaPurge(ctx, ref[0], ref[1], w.storage, w.now)
				settled = e == nil
			}
		}
		if ctx.Err() != nil {
			return b, ctx.Err()
		}
		if err = w.finish(ctx, ref[0], ref[1], until, e != nil); err != nil {
			return b, err
		}
		switch {
		case errors.Is(e, ErrLease):
			b.Contended++
		case e != nil:
			b.Deferred++
		case settled:
			b.Settled++
		default:
			b.Advanced++
		}
	}
	return b, nil
}

func (w *AccountMediaErasureWorker) Run(ctx context.Context, interval time.Duration, observe func(AccountMediaErasureBatch, bool)) error {
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
