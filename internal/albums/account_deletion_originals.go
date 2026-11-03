package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

const originalDeletionLease = 2 * time.Minute

type originalDeletionCheck struct {
	account, request, upload, resource, digest, lease string
	consumed                                          time.Time
	attempts                                          int
}
type originalDeletionBatch struct{ Acknowledged, Deferred, Contended int }

// Observe only consumed, non-legacy COPY intents. Positive receipts already in
// the journal need no cloud read. Absence or elapsed time never closes an intent.
func (r *MySQLUploads) runOriginalDeletionBatch(ctx context.Context, reconcile func(context.Context, string) (string, error), limit int, now func() time.Time) (originalDeletionBatch, error) {
	var result originalDeletionBatch
	if reconcile == nil || now == nil || limit < 1 || limit > 100 {
		return result, ErrInvalidManifest
	}
	at := now().UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return result, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT u.owner_id,d.request_id,s.upload_id,s.resource_id,s.manifest_digest,s.consumed_at FROM album_original_seals s JOIN album_uploads u ON u.upload_id=s.upload_id JOIN album_storage_accounts a ON a.owner_id=u.owner_id JOIN album_account_deletions d ON d.account_id=u.owner_id WHERE a.deletion_requested_at=d.requested_at AND s.object_version IS NULL AND s.legacy=FALSE AND COALESCE(s.deletion_check_after,s.consumed_at)<=? AND (s.deletion_check_until IS NULL OR s.deletion_check_until<=?) ORDER BY COALESCE(s.deletion_check_after,s.consumed_at),s.upload_id,s.resource_id LIMIT ?`, at, at, limit)
	if err != nil {
		return result, err
	}
	var checks []originalDeletionCheck
	for rows.Next() {
		var c originalDeletionCheck
		if err = rows.Scan(&c.account, &c.request, &c.upload, &c.resource, &c.digest, &c.consumed); err != nil {
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
		c, err = r.claimOriginalDeletion(ctx, c, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		version, callErr := reconcile(call, sealedKey(Upload{ID: c.upload, OwnerID: c.account}, c.resource))
		expired := call.Err() != nil
		cancel()
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if callErr != nil || expired || !concreteCOSVersion(version) {
			version = ""
		}
		acknowledged, err := r.finishOriginalDeletion(ctx, c, version, now().UTC())
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

// Existing media mutations acquire storage -> upload -> seal. Taking the account
// first here matches deletion acceptance without reversing that media lock order.
func (r *MySQLUploads) lockDeletionOriginal(ctx context.Context, tx *sql.Tx, c originalDeletionCheck) (string, error) {
	if !validOwner(c.account) || !validOwner(c.request) || !validOwner(c.upload) || !resourceIDPattern.MatchString(c.resource) || !validSHA256(c.digest) || c.consumed.IsZero() {
		return "", ErrConflict
	}
	if err := lockDeletingAccount(ctx, tx, c.account, c.request); err != nil {
		return "", err
	}
	if _, err := lockAccount(ctx, tx, c.account); err != nil {
		return "", err
	}
	var requested, fenced sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT d.requested_at,s.deletion_requested_at FROM album_account_deletions d JOIN album_storage_accounts s ON s.owner_id=d.account_id WHERE d.account_id=? AND d.request_id=?`, c.account, c.request).Scan(&requested, &fenced); err != nil {
		return "", err
	}
	if !requested.Valid || !fenced.Valid || !requested.Time.Equal(fenced.Time) {
		return "", ErrConflict
	}
	u, err := scanUpload(tx.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM album_uploads WHERE owner_id=? AND upload_id=? FOR UPDATE`, c.account, c.upload))
	if err != nil {
		return "", err
	}
	if u.Digest != c.digest || !sealResource(u, c.resource) {
		return "", ErrConflict
	}
	var digest string
	var version sql.NullString
	var consumed time.Time
	var legacy bool
	if err = tx.QueryRowContext(ctx, `SELECT manifest_digest,consumed_at,object_version,legacy FROM album_original_seals WHERE upload_id=? AND resource_id=? FOR UPDATE`, c.upload, c.resource).Scan(&digest, &consumed, &version, &legacy); err != nil {
		return "", err
	}
	if legacy || digest != c.digest || !consumed.Equal(c.consumed) || version.Valid && !concreteCOSVersion(version.String) {
		return "", ErrConflict
	}
	return version.String, nil
}

func (r *MySQLUploads) claimOriginalDeletion(ctx context.Context, c originalDeletionCheck, at time.Time) (originalDeletionCheck, error) {
	if at.IsZero() {
		return c, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	version, err := r.lockDeletionOriginal(ctx, tx, c)
	if err != nil {
		return c, err
	}
	if version != "" {
		return c, ErrConflict
	}
	c.lease = uuid.NewString()
	result, err := tx.ExecContext(ctx, `UPDATE album_original_seals SET deletion_check_token=?,deletion_check_until=?,deletion_check_attempts=LEAST(deletion_check_attempts+1,1000000) WHERE upload_id=? AND resource_id=? AND object_version IS NULL AND legacy=FALSE AND COALESCE(deletion_check_after,consumed_at)<=? AND (deletion_check_until IS NULL OR deletion_check_until<=?)`, c.lease, at.Add(originalDeletionLease), c.upload, c.resource, at, at)
	if err != nil {
		return c, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return c, ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT deletion_check_attempts FROM album_original_seals WHERE upload_id=? AND resource_id=?`, c.upload, c.resource).Scan(&c.attempts); err != nil {
		return c, err
	}
	return c, tx.Commit()
}

func (r *MySQLUploads) finishOriginalDeletion(ctx context.Context, c originalDeletionCheck, observed string, at time.Time) (bool, error) {
	if !validOwner(c.lease) || c.attempts < 1 || at.IsZero() || observed != "" && !concreteCOSVersion(observed) {
		return false, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stored, err := r.lockDeletionOriginal(ctx, tx, c)
	if err != nil {
		return false, err
	}
	if stored != "" && observed != "" && stored != observed {
		return false, ErrConflict
	}
	if stored != "" {
		observed = stored
	}
	var receipt any
	if observed != "" {
		receipt = observed
	}
	result, err := tx.ExecContext(ctx, `UPDATE album_original_seals SET object_version=?,deletion_check_after=?,deletion_check_token=NULL,deletion_check_until=NULL WHERE upload_id=? AND resource_id=? AND deletion_check_token=? AND deletion_check_until>?`, receipt, at.Add(accountDeletionRetry(c.attempts)), c.upload, c.resource, c.lease, at)
	if err != nil {
		return false, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return false, ErrConflict
	}
	return observed != "", tx.Commit()
}
