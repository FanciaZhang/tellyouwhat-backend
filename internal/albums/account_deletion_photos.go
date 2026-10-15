package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

const photoDeletionLease = 2 * time.Minute

type photoDeletionCheck struct {
	account, request, attempt, lease string
	attempts                         int
	record                           PhotoWriteRecord
}
type photoDeletionBatch struct{ Acknowledged, Deferred, Contended int }

// An existing write record consumes one PUT. Observe only unresolved writes;
// verified previews and durable retirement plans retain their existing evidence.
func (r *MySQLUploads) runPhotoDeletionBatch(ctx context.Context, observe func(context.Context, PhotoWriteSpec) (string, error), limit int, now func() time.Time) (photoDeletionBatch, error) {
	var result photoDeletionBatch
	if observe == nil || now == nil || limit < 1 || limit > 100 {
		return result, ErrInvalidManifest
	}
	at := now().UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return result, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT p.owner_id,d.request_id,p.attempt_id FROM album_photo_writes p JOIN album_storage_accounts a ON a.owner_id=p.owner_id JOIN album_account_deletions d ON d.account_id=p.owner_id WHERE a.deletion_requested_at=d.requested_at AND p.object_version IS NULL AND p.deletion_object_version IS NULL AND p.retirement_started_at IS NULL AND p.retired_at IS NULL AND COALESCE(p.deletion_check_after,d.requested_at)<=? AND (p.deletion_check_until IS NULL OR p.deletion_check_until<=?) ORDER BY COALESCE(p.deletion_check_after,d.requested_at),p.attempt_id LIMIT ?`, at, at, limit)
	if err != nil {
		return result, err
	}
	var checks []photoDeletionCheck
	for rows.Next() {
		var c photoDeletionCheck
		if err = rows.Scan(&c.account, &c.request, &c.attempt); err != nil {
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
		c, err = r.claimPhotoDeletion(ctx, c, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		version, callErr := observe(call, c.record.Spec)
		expired := call.Err() != nil
		cancel()
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if callErr != nil || expired || !concreteCOSVersion(version) {
			version = ""
		}
		ack, err := r.finishPhotoDeletion(ctx, c, version, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		if ack {
			result.Acknowledged++
		} else {
			result.Deferred++
		}
	}
	return result, nil
}

func (r *MySQLUploads) lockDeletionPhoto(ctx context.Context, tx *sql.Tx, c photoDeletionCheck) (PhotoWriteRecord, string, error) {
	if !validOwner(c.account) || !validOwner(c.request) || !validOwner(c.attempt) {
		return PhotoWriteRecord{}, "", ErrConflict
	}
	if err := lockDeletingAccount(ctx, tx, c.account, c.request); err != nil {
		return PhotoWriteRecord{}, "", err
	}
	if _, err := lockAccount(ctx, tx, c.account); err != nil {
		return PhotoWriteRecord{}, "", err
	}
	var requested, fenced sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT d.requested_at,s.deletion_requested_at FROM album_account_deletions d JOIN album_storage_accounts s ON s.owner_id=d.account_id WHERE d.account_id=? AND d.request_id=?`, c.account, c.request).Scan(&requested, &fenced); err != nil {
		return PhotoWriteRecord{}, "", err
	}
	if !requested.Valid || !fenced.Valid || !requested.Time.Equal(fenced.Time) {
		return PhotoWriteRecord{}, "", ErrConflict
	}
	p, err := scanPhotoWrite(tx.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND attempt_id=? FOR UPDATE`, c.account, c.attempt))
	if err != nil {
		return p, "", err
	}
	if !p.RetirementStartedAt.IsZero() || !p.RetiredAt.IsZero() {
		return p, "", ErrConflict
	}
	v, err := scanAssetVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND version_id=?`, c.account, p.VersionID))
	if err != nil {
		return p, "", err
	}
	if v.ManifestDigest != p.ManifestDigest || v.Manifest.Kind != Photo && v.Manifest.Kind != LivePhoto {
		return p, "", ErrConflict
	}
	var observed sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT deletion_object_version FROM album_photo_writes WHERE attempt_id=?`, c.attempt).Scan(&observed); err != nil {
		return p, "", err
	}
	if observed.Valid && (!concreteCOSVersion(observed.String) || p.ObjectVersion != "" && p.ObjectVersion != observed.String) {
		return p, "", ErrConflict
	}
	return p, observed.String, nil
}

func (r *MySQLUploads) claimPhotoDeletion(ctx context.Context, c photoDeletionCheck, at time.Time) (photoDeletionCheck, error) {
	if at.IsZero() {
		return c, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	var observed string
	c.record, observed, err = r.lockDeletionPhoto(ctx, tx, c)
	if err != nil {
		return c, err
	}
	if c.record.ObjectVersion != "" || observed != "" {
		return c, ErrConflict
	}
	c.lease = uuid.NewString()
	result, err := tx.ExecContext(ctx, `UPDATE album_photo_writes SET deletion_check_token=?,deletion_check_until=?,deletion_check_attempts=LEAST(deletion_check_attempts+1,1000000) WHERE attempt_id=? AND (deletion_check_after IS NULL OR deletion_check_after<=?) AND (deletion_check_until IS NULL OR deletion_check_until<=?)`, c.lease, at.Add(photoDeletionLease), c.attempt, at, at)
	if err != nil {
		return c, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return c, ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT deletion_check_attempts FROM album_photo_writes WHERE attempt_id=?`, c.attempt).Scan(&c.attempts); err != nil {
		return c, err
	}
	return c, tx.Commit()
}

func (r *MySQLUploads) finishPhotoDeletion(ctx context.Context, c photoDeletionCheck, observed string, at time.Time) (bool, error) {
	if !validOwner(c.lease) || c.attempts < 1 || at.IsZero() || observed != "" && !concreteCOSVersion(observed) {
		return false, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	p, stored, err := r.lockDeletionPhoto(ctx, tx, c)
	if err != nil {
		return false, err
	}
	if p.VersionID != c.record.VersionID || p.ManifestDigest != c.record.ManifestDigest || p.Spec != c.record.Spec {
		return false, ErrConflict
	}
	if p.ObjectVersion != "" {
		stored = p.ObjectVersion
	}
	if stored != "" && observed != "" && stored != observed {
		return false, ErrConflict
	}
	if stored != "" {
		observed = stored
	}
	var version any
	if observed != "" {
		version = observed
	}
	result, err := tx.ExecContext(ctx, `UPDATE album_photo_writes SET deletion_object_version=?,deletion_check_after=?,deletion_check_token=NULL,deletion_check_until=NULL WHERE attempt_id=? AND deletion_check_token=? AND deletion_check_until>?`, version, at.Add(accountDeletionRetry(c.attempts)), c.attempt, c.lease, at)
	if err != nil {
		return false, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return false, ErrConflict
	}
	return observed != "", tx.Commit()
}
