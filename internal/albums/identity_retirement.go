package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The extra skew covers assertions issued slightly ahead of the server clock.
// Registration rechecks assertion age after taking this same identity lock.
const identityFenceRetention = appleIdentityMaxAge + appleIdentityClockSkew

func (r *MySQLUploads) retireIdentityFence(ctx context.Context, subject string, now func() time.Time) (bool, error) {
	if !validSHA256(subject) || now == nil {
		return false, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var revoked, deleted sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT revoked_before,deleted_at FROM album_apple_identity_state WHERE subject_sha256=? FOR UPDATE`, subject).Scan(&revoked, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = lockRecoveryState(ctx, tx); err != nil {
		return false, err
	}
	at := now().UTC()
	if at.IsZero() {
		return false, ErrInvalidManifest
	}
	cutoff := at.Add(-identityFenceRetention)
	if !revoked.Valid || !revoked.Time.Before(cutoff) || deleted.Valid && !deleted.Time.Before(cutoff) {
		return false, nil
	}
	var owner string
	err = tx.QueryRowContext(ctx, `SELECT account_id FROM album_accounts WHERE apple_subject_sha256=? FOR UPDATE`, subject).Scan(&owner)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM album_apple_identity_state WHERE subject_sha256=?`, subject); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (r *MySQLUploads) runIdentityRetirementBatch(ctx context.Context, limit int, now func() time.Time) (int, error) {
	if limit < 1 || limit > 100 || now == nil || now().IsZero() {
		return 0, ErrInvalidManifest
	}
	cutoff := now().UTC().Add(-identityFenceRetention)
	rows, err := r.db.QueryContext(ctx, `SELECT i.subject_sha256 FROM album_apple_identity_state i
 WHERE i.revoked_before<? AND (i.deleted_at IS NULL OR i.deleted_at<?)
 AND NOT EXISTS (SELECT 1 FROM album_accounts a WHERE a.apple_subject_sha256=i.subject_sha256)
 ORDER BY i.revoked_before,i.subject_sha256 LIMIT ?`, cutoff, cutoff, limit)
	if err != nil {
		return 0, err
	}
	var subjects []string
	for rows.Next() {
		var subject string
		if err = rows.Scan(&subject); err != nil {
			rows.Close()
			return 0, err
		}
		subjects = append(subjects, subject)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, subject := range subjects {
		removed, err := r.retireIdentityFence(ctx, subject, now)
		if err != nil {
			return count, err
		}
		if removed {
			count++
		}
	}
	return count, nil
}
