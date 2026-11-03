package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

const livePairDeletionLease = 2 * time.Minute

type livePairDeletionCheck struct {
	account, request, id, lease string
	attempts                    int
	pair                        LivePairUpload
}
type livePairDeletionObservation struct{ photo, movie string }
type livePairDeletionBatch struct{ Acknowledged, Deferred, Contended int }

func missingLivePairCopy(u LivePairUpload) bool {
	return u.PhotoConsumed && u.PhotoCopyVersion == "" || u.MovieConsumed && u.MovieCopyVersion == ""
}

// Observe only previously issued COPYs. Missing objects and elapsed time never
// prove completion, and no new COPY or media deletion is issued by this phase.
func (r *MySQLUploads) runLivePairDeletionBatch(ctx context.Context, observe func(context.Context, LivePairUpload, string) (string, error), limit int, now func() time.Time) (livePairDeletionBatch, error) {
	var result livePairDeletionBatch
	if observe == nil || now == nil || limit < 1 || limit > 100 {
		return result, ErrInvalidManifest
	}
	at := now().UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return result, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT p.owner_id,d.request_id,p.pair_upload_id FROM album_live_pairs p JOIN album_storage_accounts a ON a.owner_id=p.owner_id JOIN album_account_deletions d ON d.account_id=p.owner_id WHERE a.deletion_requested_at=d.requested_at AND ((p.photo_consumed=TRUE AND p.photo_copy_version IS NULL) OR (p.movie_consumed=TRUE AND p.movie_copy_version IS NULL)) AND COALESCE(p.deletion_check_after,d.requested_at)<=? AND (p.deletion_check_until IS NULL OR p.deletion_check_until<=?) ORDER BY COALESCE(p.deletion_check_after,d.requested_at),p.pair_upload_id LIMIT ?`, at, at, limit)
	if err != nil {
		return result, err
	}
	var checks []livePairDeletionCheck
	for rows.Next() {
		var c livePairDeletionCheck
		if err = rows.Scan(&c.account, &c.request, &c.id); err != nil {
			rows.Close()
			return result, err
		}
		checks = append(checks, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, c := range checks {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		c, err = r.claimLivePairDeletion(ctx, c, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		var observation livePairDeletionObservation
		for _, part := range []string{"photo", "motion"} {
			consumed, stored := c.pair.PhotoConsumed, c.pair.PhotoCopyVersion
			if part == "motion" {
				consumed, stored = c.pair.MovieConsumed, c.pair.MovieCopyVersion
			}
			if !consumed || stored != "" {
				continue
			}
			call, cancel := context.WithTimeout(ctx, 30*time.Second)
			version, callErr := observe(call, c.pair, part)
			expired := call.Err() != nil
			cancel()
			if err = ctx.Err(); err != nil {
				return result, err
			}
			if callErr != nil || expired || !concreteCOSVersion(version) {
				continue
			}
			if part == "photo" {
				observation.photo = version
			} else {
				observation.movie = version
			}
		}
		acknowledged, err := r.finishLivePairDeletion(ctx, c, observation, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		if acknowledged {
			result.Acknowledged++
		} else {
			result.Deferred++
		}
	}
	return result, nil
}

func (r *MySQLUploads) lockDeletionLivePair(ctx context.Context, tx *sql.Tx, c livePairDeletionCheck) (LivePairUpload, error) {
	if !validOwner(c.account) || !validOwner(c.request) || !validOwner(c.id) {
		return LivePairUpload{}, ErrConflict
	}
	// Match deletion acceptance: account -> storage -> media.
	if err := lockDeletingAccount(ctx, tx, c.account, c.request); err != nil {
		return LivePairUpload{}, err
	}
	if _, err := lockAccount(ctx, tx, c.account); err != nil {
		return LivePairUpload{}, err
	}
	var requested, fenced sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT d.requested_at,s.deletion_requested_at FROM album_account_deletions d JOIN album_storage_accounts s ON s.owner_id=d.account_id WHERE d.account_id=? AND d.request_id=?`, c.account, c.request).Scan(&requested, &fenced); err != nil {
		return LivePairUpload{}, err
	}
	if !requested.Valid || !fenced.Valid || !requested.Time.Equal(fenced.Time) {
		return LivePairUpload{}, ErrConflict
	}
	return scanLivePair(tx.QueryRowContext(ctx, `SELECT `+livePairColumns+` FROM album_live_pairs WHERE owner_id=? AND pair_upload_id=? FOR UPDATE`, c.account, c.id))
}

func (r *MySQLUploads) claimLivePairDeletion(ctx context.Context, c livePairDeletionCheck, at time.Time) (livePairDeletionCheck, error) {
	if at.IsZero() {
		return c, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	c.pair, err = r.lockDeletionLivePair(ctx, tx, c)
	if err != nil {
		return c, err
	}
	if !missingLivePairCopy(c.pair) {
		return c, ErrConflict
	}
	c.lease = uuid.NewString()
	result, err := tx.ExecContext(ctx, `UPDATE album_live_pairs SET deletion_check_token=?,deletion_check_until=?,deletion_check_attempts=LEAST(deletion_check_attempts+1,1000000) WHERE pair_upload_id=? AND (deletion_check_after IS NULL OR deletion_check_after<=?) AND (deletion_check_until IS NULL OR deletion_check_until<=?)`, c.lease, at.Add(livePairDeletionLease), c.id, at, at)
	if err != nil {
		return c, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return c, ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT deletion_check_attempts FROM album_live_pairs WHERE pair_upload_id=?`, c.id).Scan(&c.attempts); err != nil {
		return c, err
	}
	return c, tx.Commit()
}

func (r *MySQLUploads) finishLivePairDeletion(ctx context.Context, c livePairDeletionCheck, observation livePairDeletionObservation, at time.Time) (bool, error) {
	if !validOwner(c.lease) || c.attempts < 1 || at.IsZero() || observation.photo != "" && !concreteCOSVersion(observation.photo) || observation.movie != "" && !concreteCOSVersion(observation.movie) {
		return false, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	current, err := r.lockDeletionLivePair(ctx, tx, c)
	if err != nil {
		return false, err
	}
	if !sameLivePairCopy(current, c.pair) || current.PhotoConsumed != c.pair.PhotoConsumed || current.MovieConsumed != c.pair.MovieConsumed {
		return false, ErrConflict
	}
	for _, part := range []string{"photo", "motion"} {
		version := observation.photo
		if part == "motion" {
			version = observation.movie
		}
		if version == "" {
			continue
		}
		current, err = recordLivePairSeal(ctx, tx, current, c.pair, part, version)
		if err != nil {
			return false, err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE album_live_pairs SET deletion_check_after=?,deletion_check_token=NULL,deletion_check_until=NULL WHERE pair_upload_id=? AND deletion_check_token=? AND deletion_check_until>?`, at.Add(accountDeletionRetry(c.attempts)), c.id, c.lease, at)
	if err != nil {
		return false, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return false, ErrConflict
	}
	return !missingLivePairCopy(current), tx.Commit()
}
