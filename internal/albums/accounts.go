package albums

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
)

// Implemented by the existing AES-GCM PayloadCipher. Album credentials need
// their own deployment key and must not be persisted through an identity cipher.
type AccountCipher interface {
	Encrypt([]byte, []byte) (ciphertext, nonce []byte, err error)
	Decrypt(ciphertext, nonce, associatedData []byte) ([]byte, error)
}

type AccountLogin struct{ AccountID, GrantID string }
type MySQLAccounts struct {
	db     *sql.DB
	cipher AccountCipher
}

func NewMySQLAccounts(db *sql.DB, cipher AccountCipher) (*MySQLAccounts, error) {
	if db == nil || cipher == nil {
		return nil, errors.New("album account database and credential cipher are required")
	}
	return &MySQLAccounts{db, cipher}, nil
}
func appleSubjectHash(subject string) string {
	hash := sha256.Sum256([]byte(appleIssuer + "\x00" + albumAppleAudience + "\x00" + subject))
	return hex.EncodeToString(hash[:])
}
func appleGrantAAD(account, subjectHash, grant string) []byte {
	return []byte("albums/apple-refresh/v1\x00" + account + "\x00" + subjectHash + "\x00" + grant)
}

// RegisterAppleLogin accepts only a verified internal AppleGrant. The API must
// never decode this type from a caller's JSON. Subject and quota are not client
// choices. Each distinct Apple refresh grant is retained for later revocation.
func (r *MySQLAccounts) RegisterAppleLogin(ctx context.Context, grant AppleGrant) (AccountLogin, error) {
	if grant.Identity.Subject == "" || len(grant.Identity.Subject) > 255 || grant.RefreshToken == "" || len(grant.RefreshToken) > 16<<10 {
		return AccountLogin{}, ErrAppleIdentity
	}
	subjectHash := appleSubjectHash(grant.Identity.Subject)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return AccountLogin{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES (?,?) ON DUPLICATE KEY UPDATE account_id=account_id`, uuid.NewString(), subjectHash)
	if err != nil {
		return AccountLogin{}, err
	}
	var account, state string
	if err := tx.QueryRowContext(ctx, `SELECT account_id,state FROM album_accounts WHERE apple_subject_sha256=? FOR UPDATE`, subjectHash).Scan(&account, &state); err != nil {
		return AccountLogin{}, err
	}
	if state != "active" {
		return AccountLogin{}, ErrOwner
	}
	// A sign-in cannot reset a paid account's quota or grant free cloud capacity.
	_, err = tx.ExecContext(ctx, `INSERT INTO album_storage_accounts(owner_id,quota_bytes) VALUES (?,0) ON DUPLICATE KEY UPDATE owner_id=owner_id`, account)
	if err != nil {
		return AccountLogin{}, err
	}
	id := uuid.NewString()
	ciphertext, nonce, err := r.cipher.Encrypt([]byte(grant.RefreshToken), appleGrantAAD(account, subjectHash, id))
	if err != nil || len(nonce) != 12 || len(ciphertext) <= len(grant.RefreshToken) {
		return AccountLogin{}, errors.New("album credential encryption failed")
	}
	digest := sha256.Sum256([]byte(grant.RefreshToken))
	_, err = tx.ExecContext(ctx, `INSERT INTO album_apple_grants(grant_id,account_id,token_sha256,encrypted_token,token_nonce) VALUES (?,?,?,?,?) ON DUPLICATE KEY UPDATE grant_id=grant_id`, id, account, digest[:], ciphertext, nonce)
	if err != nil {
		return AccountLogin{}, err
	}
	var revoked sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT grant_id,revoked_at FROM album_apple_grants WHERE account_id=? AND token_sha256=? FOR UPDATE`, account, digest[:]).Scan(&id, &revoked); err != nil {
		return AccountLogin{}, err
	}
	if revoked.Valid {
		return AccountLogin{}, ErrAppleCode
	}
	if err := tx.Commit(); err != nil {
		return AccountLogin{}, err
	}
	return AccountLogin{AccountID: account, GrantID: id}, nil
}

// RefreshCredential is server-only, for provider validation/revocation. It
// cannot authenticate a client; app bearer sessions are separate credentials.
func (r *MySQLAccounts) RefreshCredential(ctx context.Context, account, grant string) (string, error) {
	if !validOwner(account) || !validOwner(grant) {
		return "", ErrOwner
	}
	var encrypted, nonce []byte
	var subjectHash string
	err := r.db.QueryRowContext(ctx, `SELECT g.encrypted_token,g.token_nonce,a.apple_subject_sha256 FROM album_apple_grants g JOIN album_accounts a ON a.account_id=g.account_id WHERE g.account_id=? AND g.grant_id=?`, account, grant).Scan(&encrypted, &nonce, &subjectHash)
	if err != nil {
		return "", ErrOwner
	}
	if len(nonce) != 12 {
		return "", ErrAppleCode
	}
	plaintext, err := r.cipher.Decrypt(encrypted, nonce, appleGrantAAD(account, subjectHash, grant))
	if err != nil {
		return "", ErrAppleCode
	}
	return string(plaintext), nil
}
