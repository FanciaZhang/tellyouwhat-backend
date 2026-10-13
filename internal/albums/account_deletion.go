package albums

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"
)

type accountDeletionRequest struct {
	AccountID, RequestID string
	RequestedAt          time.Time
}

// Durable access cutover, not evidence of erasure. No HTTP route is exposed
// until media quiescence, provider revocation and physical cleanup are wired.
// The owner is derived from the credential, never a caller-supplied account ID.
func (r *MySQLAccounts) requestAccountDeletion(ctx context.Context, token, requestID string, now func() time.Time) (accountDeletionRequest, error) {
	var receipt accountDeletionRequest
	digest, err := sessionDigest(token)
	if err != nil || !validOwner(requestID) || now == nil {
		return receipt, ErrSession
	}
	var owner, subject string
	err = r.db.QueryRowContext(ctx, `SELECT a.account_id,a.apple_subject_sha256 FROM album_sessions s JOIN album_accounts a ON a.account_id=s.account_id WHERE s.token_sha256=?`, digest[:]).Scan(&owner, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		// Access records may already be erased. The accepted request retains only
		// the authorizing token hash, valid solely for this exact request replay.
		var acceptedDigest []byte
		err = r.db.QueryRowContext(ctx, `SELECT d.account_id,d.request_id,d.requested_at,d.authorizing_session_sha256
 FROM album_account_deletions d JOIN album_accounts a ON a.account_id=d.account_id
 WHERE d.request_id=? AND d.access_erased_at IS NOT NULL AND a.state='deleting'`, requestID).Scan(&receipt.AccountID, &receipt.RequestID, &receipt.RequestedAt, &acceptedDigest)
		if errors.Is(err, sql.ErrNoRows) {
			return accountDeletionRequest{}, ErrSession
		}
		if err != nil {
			return accountDeletionRequest{}, err
		}
		if subtle.ConstantTimeCompare(digest[:], acceptedDigest) != 1 {
			return accountDeletionRequest{}, ErrSession
		}
		return receipt, nil
	}
	if err != nil {
		return receipt, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	// Match Apple registration/notifications: identity, account, then storage.
	if _, _, err = lockAppleIdentity(ctx, tx, subject); err != nil {
		return receipt, err
	}
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM album_accounts WHERE account_id=? AND apple_subject_sha256=? FOR UPDATE`, owner, subject).Scan(&state); err != nil {
		return receipt, err
	}
	var oldDigest []byte
	err = tx.QueryRowContext(ctx, `SELECT account_id,request_id,authorizing_session_sha256,requested_at FROM album_account_deletions WHERE account_id=? FOR UPDATE`, owner).Scan(&receipt.AccountID, &receipt.RequestID, &oldDigest, &receipt.RequestedAt)
	if err == nil {
		// Only the exact accepted request may be replayed after revocation/expiry.
		if state != "deleting" || receipt.RequestID != requestID || subtle.ConstantTimeCompare(digest[:], oldDigest) != 1 {
			return accountDeletionRequest{}, ErrSession
		}
		return receipt, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	if state != "active" {
		return receipt, ErrSession
	}
	if _, err = lockAccount(ctx, tx, owner); err != nil {
		return receipt, err
	}
	var expiry time.Time
	var revoked, grantRevoked sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT s.expires_at,s.revoked_at,g.revoked_at FROM album_sessions s JOIN album_apple_grants g ON g.grant_id=s.grant_id AND g.account_id=s.account_id WHERE s.account_id=? AND s.token_sha256=? FOR UPDATE`, owner, digest[:]).Scan(&expiry, &revoked, &grantRevoked); err != nil {
		return receipt, err
	}
	at := now().UTC().Truncate(time.Microsecond)
	if at.IsZero() || !expiry.After(at) || revoked.Valid || grantRevoked.Valid {
		return receipt, ErrSession
	}
	receipt = accountDeletionRequest{owner, requestID, at}
	if _, err = tx.ExecContext(ctx, `INSERT INTO album_account_deletions(account_id,request_id,authorizing_session_sha256,requested_at) VALUES (?,?,?,?)`, owner, requestID, digest[:], at); err != nil {
		return accountDeletionRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_accounts SET state='deleting' WHERE account_id=?`, owner); err != nil {
		return accountDeletionRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_storage_accounts SET deletion_requested_at=? WHERE owner_id=?`, at, owner); err != nil {
		return accountDeletionRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_sessions SET revoked_at=COALESCE(revoked_at,?) WHERE account_id=?`, at, owner); err != nil {
		return accountDeletionRequest{}, err
	}
	// This is local grant revocation. Encrypted provider tokens remain available
	// to the eventual Apple revocation worker; no provider success is implied.
	if _, err = tx.ExecContext(ctx, `UPDATE album_apple_grants SET revoked_at=COALESCE(revoked_at,?) WHERE account_id=?`, at, owner); err != nil {
		return accountDeletionRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO album_deletion_apple_jobs(grant_id,account_id,request_id,available_at) SELECT grant_id,account_id,?,? FROM album_apple_grants WHERE account_id=?`, requestID, at, owner); err != nil {
		return accountDeletionRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO album_deletion_video_jobs(job_id,account_id,request_id,available_at) SELECT job_id,owner_id,?,? FROM album_video_jobs WHERE owner_id=? AND spec_json IS NOT NULL`, requestID, at, owner); err != nil {
		return accountDeletionRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_share_links SET revoked_at=COALESCE(revoked_at,?) WHERE owner_id=?`, at, owner); err != nil {
		return accountDeletionRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_push_registrations p JOIN album_sessions s ON s.session_id=p.session_id SET p.state='disabled',p.revision=p.revision+1,p.token_sha256=NULL,p.encrypted_token=NULL,p.token_nonce=NULL,p.registered_at=NULL,p.request_id=NULL,p.request_sha256=NULL WHERE s.account_id=?`, owner); err != nil {
		return accountDeletionRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_retention_reminders SET state='cancelled',lease_token=NULL,lease_until=NULL WHERE owner_id=? AND state='pending'`, owner); err != nil {
		return accountDeletionRequest{}, err
	}
	if err = tx.Commit(); err != nil {
		return accountDeletionRequest{}, err
	}
	return receipt, nil
}
