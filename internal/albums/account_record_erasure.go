package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// This receipt covers online account records, not historical backups or the
// provider revocation fence. The independent deletion ledger must survive both.
func (r *MySQLUploads) eraseAccountRecords(ctx context.Context, owner, request string, now func() time.Time) (time.Time, error) {
	if now == nil {
		return time.Time{}, ErrInvalidManifest
	}
	at := now().UTC().Truncate(time.Microsecond)
	if !validOwner(owner) || !validOwner(request) || at.IsZero() || (r.billingEnvironment != "Sandbox" && r.billingEnvironment != "Production") {
		return time.Time{}, ErrInvalidManifest
	}
	var subject string
	err := r.db.QueryRowContext(ctx, `SELECT apple_subject_sha256 FROM album_accounts WHERE account_id=?`, owner).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return r.erasedAccountRecords(ctx, owner, request)
	}
	if err != nil {
		return time.Time{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	if _, _, err = lockAppleIdentity(ctx, tx, subject); err != nil {
		return time.Time{}, err
	}
	if err = lockAccountPurgeScope(ctx, tx, owner, request, at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Another eraser may have completed while we waited for the identity.
			tx.Rollback()
			return r.erasedAccountRecords(ctx, owner, request)
		}
		return time.Time{}, err
	}
	// Evaluate freshness after acquiring locks, not before a potentially long wait.
	at = now().UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return time.Time{}, ErrInvalidManifest
	}
	var access, media, checked sql.NullTime
	var checkedRevision sql.NullInt64
	var revision, used, reserved int64
	err = tx.QueryRowContext(ctx, `SELECT d.access_erased_at,p.media_settled_at,s.subscription_checked_revision,s.subscription_checked_at,s.subscription_revision,s.used_bytes,s.reserved_bytes
 FROM album_account_deletions d JOIN album_account_object_purges p ON p.account_id=d.account_id AND p.request_id=d.request_id AND p.state='objects_removed'
 JOIN album_storage_accounts s ON s.owner_id=d.account_id
 JOIN album_deletion_ledger l ON l.account_id=d.account_id AND l.request_id=d.request_id AND l.requested_at=d.requested_at AND l.authorizing_session_sha256=d.authorizing_session_sha256
 WHERE d.account_id=? AND d.request_id=? AND l.records_erased_at IS NULL FOR UPDATE`, owner, request).
		Scan(&access, &media, &checkedRevision, &checked, &revision, &used, &reserved)
	if err != nil {
		return time.Time{}, err
	}
	if !access.Valid || !media.Valid || at.Before(access.Time) || at.Before(media.Time) || used != 0 || reserved != 0 {
		return time.Time{}, ErrDerivedPending
	}
	var chains, unresolved int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(c.environment<>? OR b.account_id IS NULL OR b.account_id<>c.owner_id OR b.request_id<>? OR b.target_token<>a.storekit_account_token OR b.acknowledged_at IS NULL OR b.verified_signed_at<c.signed_at),0)
 FROM album_cloud_subscriptions c JOIN album_accounts a ON a.account_id=c.owner_id
 LEFT JOIN album_deletion_subscription_jobs b ON b.environment=c.environment AND b.original_transaction_id=c.original_transaction_id WHERE c.owner_id=?`, r.billingEnvironment, request, owner).Scan(&chains, &unresolved)
	if err != nil {
		return time.Time{}, err
	}
	if chains == 0 && revision != 0 || unresolved != 0 {
		return time.Time{}, ErrDerivedPending
	}
	if chains > 0 && (!checkedRevision.Valid || checkedRevision.Int64 != revision || !checked.Valid || at.Before(checked.Time) || !at.Before(checked.Time.Add(time.Minute))) {
		// Settlement may finish long after binding. Reopen the complete set instead
		// of treating a once-valid receipt as permanent permission to erase it.
		if _, err = tx.ExecContext(ctx, `UPDATE album_storage_accounts SET subscription_checked_revision=NULL,subscription_checked_at=NULL WHERE owner_id=?`, owner); err != nil {
			return time.Time{}, err
		}
		if err = enqueueSubscriptionBindings(ctx, tx, owner, "", "", at); err != nil {
			return time.Time{}, err
		}
		if err = tx.Commit(); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, ErrDerivedPending
	}
	// Refuse leftover rows, including tables whose FK would otherwise cascade.
	for _, table := range []string{"album_uploads", "album_asset_versions", "album_asset_lifecycle", "album_library_changes", "album_asset_operations", "album_share_links", "album_retention_reminders"} {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE owner_id=?`, owner).Scan(&count); err != nil {
			return time.Time{}, err
		}
		if count != 0 {
			return time.Time{}, ErrDerivedPending
		}
	}
	// Preserve the provider fence so an already-issued login cannot recreate the
	// account after waiting for this transaction. Fresh reauthorization may do so.
	if _, err = tx.ExecContext(ctx, `UPDATE album_apple_identity_state SET revoked_before=GREATEST(COALESCE(revoked_before,?),?) WHERE subject_sha256=?`, at, at, subject); err != nil {
		return time.Time{}, err
	}
	for _, query := range []string{
		`DELETE FROM album_deletion_subscription_jobs WHERE account_id=?`,
		`DELETE FROM album_subscription_notifications WHERE owner_id=?`,
		`DELETE FROM album_cloud_subscriptions WHERE owner_id=?`,
		`DELETE FROM album_storage_accounts WHERE owner_id=?`,
		`DELETE FROM album_account_object_purges WHERE account_id=?`,
		`DELETE FROM album_account_deletions WHERE account_id=?`,
		`DELETE FROM album_accounts WHERE account_id=?`,
	} {
		if _, err = tx.ExecContext(ctx, query, owner); err != nil {
			return time.Time{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_deletion_ledger SET records_erased_at=? WHERE account_id=? AND request_id=? AND records_erased_at IS NULL`, at, owner, request); err != nil {
		return time.Time{}, err
	}
	return at, tx.Commit()
}

func (r *MySQLUploads) erasedAccountRecords(ctx context.Context, owner, request string) (time.Time, error) {
	var at sql.NullTime
	err := r.db.QueryRowContext(ctx, `SELECT records_erased_at FROM album_deletion_ledger WHERE account_id=? AND request_id=?`, owner, request).Scan(&at)
	if err != nil {
		return time.Time{}, err
	}
	if !at.Valid {
		return time.Time{}, ErrDerivedPending
	}
	return at.Time, nil
}

func (r *MySQLUploads) runAccountRecordErasureBatch(ctx context.Context, limit int, now func() time.Time) (int, error) {
	if limit < 1 || limit > 100 || now == nil {
		return 0, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT d.account_id,d.request_id FROM album_account_deletions d
 JOIN album_account_object_purges p ON p.account_id=d.account_id AND p.request_id=d.request_id
	 JOIN album_storage_accounts s ON s.owner_id=d.account_id
	 WHERE d.access_erased_at IS NOT NULL AND p.media_settled_at IS NOT NULL
	 AND (s.subscription_checked_at IS NOT NULL OR s.subscription_revision=0)
	 AND NOT EXISTS (SELECT 1 FROM album_deletion_subscription_jobs b WHERE b.account_id=d.account_id AND b.acknowledged_at IS NULL)
	 ORDER BY d.requested_at,d.account_id LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	var refs [][2]string
	for rows.Next() {
		var ref [2]string
		if err = rows.Scan(&ref[0], &ref[1]); err != nil {
			rows.Close()
			return 0, err
		}
		refs = append(refs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, ref := range refs {
		_, err = r.eraseAccountRecords(ctx, ref[0], ref[1], now)
		if errors.Is(err, ErrDerivedPending) {
			continue
		}
		if err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
