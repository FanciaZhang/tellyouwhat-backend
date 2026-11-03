package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const unresolvedAppleAccess = `(j.acknowledged_at IS NULL OR g.revoked_at IS NULL OR OCTET_LENGTH(g.encrypted_token)<>0 OR g.token_nonce<>UNHEX(REPEAT('00',12)))`

// Identity/account and subscription records remain fenced for billing and
// backup erasure. This receipt only covers access credentials and push data.
func (r *MySQLAccounts) eraseAccountAccess(ctx context.Context, owner, request string, at time.Time) (time.Time, error) {
	at = at.UTC().Truncate(time.Microsecond)
	if !validOwner(owner) || !validOwner(request) || at.IsZero() {
		return time.Time{}, ErrInvalidManifest
	}
	var subject string
	if err := r.db.QueryRowContext(ctx, `SELECT apple_subject_sha256 FROM album_accounts WHERE account_id=?`, owner).Scan(&subject); err != nil {
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
		return time.Time{}, err
	}
	var old, media sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT access_erased_at FROM album_account_deletions WHERE account_id=? AND request_id=? FOR UPDATE`, owner, request).Scan(&old); err != nil {
		return time.Time{}, err
	}
	if old.Valid {
		return old.Time, tx.Commit()
	}
	if err = tx.QueryRowContext(ctx, `SELECT media_settled_at FROM album_account_object_purges WHERE account_id=? AND request_id=? AND state='objects_removed' FOR UPDATE`, owner, request).Scan(&media); err != nil {
		return time.Time{}, err
	}
	if !media.Valid || at.Before(media.Time) {
		return time.Time{}, ErrDerivedPending
	}
	var found int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM album_apple_grants WHERE account_id=? LIMIT 1 FOR UPDATE`, owner).Scan(&found); errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrDerivedPending
	} else if err != nil {
		return time.Time{}, err
	}
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM album_apple_grants g LEFT JOIN album_deletion_apple_jobs j
 ON j.grant_id=g.grant_id AND j.account_id=g.account_id AND j.request_id=?
 WHERE g.account_id=? AND `+unresolvedAppleAccess+` LIMIT 1 FOR UPDATE`, request, owner).Scan(&found)
	if err == nil {
		return time.Time{}, ErrDerivedPending
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	// A missing acknowledgement is not an empty queue. Preserve every credential
	// until its own provider success and credential wipe were committed together.
	for _, query := range []string{
		`DELETE d FROM album_retention_deliveries d JOIN album_retention_reminders m ON m.reminder_id=d.reminder_id WHERE m.owner_id=?`,
		`DELETE d FROM album_retention_deliveries d JOIN album_push_registrations p ON p.registration_id=d.registration_id JOIN album_sessions s ON s.session_id=p.session_id WHERE s.account_id=?`,
		`DELETE FROM album_retention_reminders WHERE owner_id=?`,
		`DELETE p FROM album_push_registrations p JOIN album_sessions s ON s.session_id=p.session_id WHERE s.account_id=?`,
		`DELETE FROM album_sessions WHERE account_id=?`,
		`DELETE FROM album_deletion_apple_jobs WHERE account_id=?`,
		`DELETE FROM album_apple_grants WHERE account_id=?`,
	} {
		if _, err = tx.ExecContext(ctx, query, owner); err != nil {
			return time.Time{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_account_deletions SET access_erased_at=? WHERE account_id=? AND request_id=?`, at, owner, request); err != nil {
		return time.Time{}, err
	}
	if err = tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return at, nil
}

func (r *MySQLAccounts) runAccessErasureBatch(ctx context.Context, limit int, now func() time.Time) (int, error) {
	if limit < 1 || limit > 100 || now == nil {
		return 0, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT d.account_id,d.request_id FROM album_account_deletions d
 JOIN album_account_object_purges p ON p.account_id=d.account_id AND p.request_id=d.request_id
 WHERE d.access_erased_at IS NULL AND p.media_settled_at IS NOT NULL
 AND EXISTS (SELECT 1 FROM album_apple_grants g WHERE g.account_id=d.account_id) AND NOT EXISTS (
 SELECT 1 FROM album_apple_grants g LEFT JOIN album_deletion_apple_jobs j
 ON j.grant_id=g.grant_id AND j.account_id=g.account_id AND j.request_id=d.request_id
 WHERE g.account_id=d.account_id AND `+unresolvedAppleAccess+`)
 ORDER BY p.media_settled_at,d.account_id LIMIT ?`, limit)
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
		if _, err = r.eraseAccountAccess(ctx, ref[0], ref[1], now()); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
