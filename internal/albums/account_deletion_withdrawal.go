package albums

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"
)

const acceptedDeletionQuery = `SELECT account_id,request_id,requested_at,authorizing_session_sha256 FROM album_deletion_ledger WHERE request_id=?`

func scanAcceptedDeletion(row *sql.Row, digest [32]byte) (accountDeletionRequest, error) {
	var receipt accountDeletionRequest
	var accepted []byte
	if err := row.Scan(&receipt.AccountID, &receipt.RequestID, &receipt.RequestedAt, &accepted); err != nil {
		return accountDeletionRequest{}, err
	}
	if subtle.ConstantTimeCompare(digest[:], accepted) != 1 {
		return accountDeletionRequest{}, ErrSession
	}
	return receipt, nil
}

// A nil receipt means withdrawal committed. An accepted receipt is immutable:
// withdrawal cannot undo erasure. Revoking the original session under the same
// account lock as acceptance prevents an in-flight request from arriving later.
func (r *MySQLAccounts) withdrawAccountDeletion(ctx context.Context, token, requestID string, now func() time.Time) (*accountDeletionRequest, error) {
	digest, err := sessionDigest(token)
	if err != nil || !validOwner(requestID) || now == nil {
		return nil, ErrSession
	}
	var owner, subject string
	err = r.db.QueryRowContext(ctx, `SELECT a.account_id,a.apple_subject_sha256 FROM album_sessions s JOIN album_accounts a ON a.account_id=s.account_id WHERE s.token_sha256=?`, digest[:]).Scan(&owner, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		receipt, e := scanAcceptedDeletion(r.db.QueryRowContext(ctx, acceptedDeletionQuery, requestID), digest)
		if errors.Is(e, sql.ErrNoRows) {
			// No session can accept a new request with this credential; session
			// tokens are random and are never reissued after removal.
			return nil, nil
		}
		if e != nil {
			return nil, e
		}
		return &receipt, nil
	}
	if err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, _, err = lockAppleIdentity(ctx, tx, subject); err != nil {
		return nil, err
	}
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM album_accounts WHERE account_id=? AND apple_subject_sha256=? FOR UPDATE`, owner, subject).Scan(&state); err != nil {
		return nil, err
	}
	receipt, err := scanAcceptedDeletion(tx.QueryRowContext(ctx, acceptedDeletionQuery+` FOR UPDATE`, requestID), digest)
	if err == nil {
		return &receipt, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// An accepted request under another credential/ID must not be cancelled.
	if state == "deleting" {
		return nil, ErrSession
	}
	if _, err = lockAccount(ctx, tx, owner); err != nil {
		return nil, err
	}
	at := now().UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return nil, ErrInvalidManifest
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_sessions SET revoked_at=COALESCE(revoked_at,?) WHERE account_id=? AND token_sha256=?`, at, owner, digest[:]); err != nil {
		return nil, err
	}
	// Clear only this session's push binding; other devices keep their access.
	if _, err = tx.ExecContext(ctx, `UPDATE album_push_registrations p JOIN album_sessions s ON s.session_id=p.session_id SET p.state='disabled',p.revision=p.revision+1,p.token_sha256=NULL,p.encrypted_token=NULL,p.token_nonce=NULL,p.registered_at=NULL,p.request_id=NULL,p.request_sha256=NULL WHERE s.account_id=? AND s.token_sha256=? AND p.state<>'disabled'`, owner, digest[:]); err != nil {
		return nil, err
	}
	return nil, tx.Commit()
}
