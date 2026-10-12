package albums

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

type shareRow struct {
	ID, Owner, Version, Request, KeyID string
	RequestDigest, TokenDigest         [32]byte
	Password                           []byte
	MaxClaims, Claims                  int
	Created, Expiry                    time.Time
	Revoked                            *time.Time
}

func (shareRow) String() string   { return "[redacted album share record]" }
func (shareRow) GoString() string { return "[redacted album share record]" }

type shareClaimRow struct {
	ID, Share, Request, KeyID string
	TokenDigest               [32]byte
	Created, Expiry           time.Time
}

func (shareClaimRow) String() string   { return "[redacted album share claim record]" }
func (shareClaimRow) GoString() string { return "[redacted album share claim record]" }

const shareColumns = `share_id,owner_id,version_id,request_id,key_id,request_digest,token_digest,password_proof,max_claims,claimed_count,created_at,expires_at,revoked_at`
const shareClaimColumns = `claim_id,share_id,request_id,key_id,token_digest,created_at,expires_at`

func scanShare(row rowScanner) (shareRow, error) {
	var s shareRow
	var request, token []byte
	var revoked sql.NullTime
	err := row.Scan(&s.ID, &s.Owner, &s.Version, &s.Request, &s.KeyID, &request, &token, &s.Password, &s.MaxClaims, &s.Claims, &s.Created, &s.Expiry, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	key, err := hex.DecodeString(s.KeyID)
	if !validOwner(s.ID) || !validOwner(s.Owner) || !validOwner(s.Version) || !validOwner(s.Request) || err != nil || len(key) != 8 || len(request) != 32 || len(token) != 32 || s.MaxClaims < 1 || s.MaxClaims > 100 || s.Claims < 0 || s.Claims > s.MaxClaims || (!s.Expiry.After(s.Created)) || (len(s.Password) != 0 && (len(s.Password) != 49 || s.Password[0] != 1)) {
		return s, ErrShareUnavailable
	}
	copy(s.RequestDigest[:], request)
	copy(s.TokenDigest[:], token)
	if revoked.Valid {
		v := revoked.Time.UTC()
		s.Revoked = &v
	}
	return s, nil
}
func scanShareClaim(row rowScanner) (shareClaimRow, error) {
	var c shareClaimRow
	var token []byte
	err := row.Scan(&c.ID, &c.Share, &c.Request, &c.KeyID, &token, &c.Created, &c.Expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	key, err := hex.DecodeString(c.KeyID)
	if !validOwner(c.ID) || !validOwner(c.Share) || !validOwner(c.Request) || err != nil || len(key) != 8 || len(token) != 32 || !c.Expiry.After(c.Created) {
		return c, ErrShareUnavailable
	}
	copy(c.TokenDigest[:], token)
	return c, nil
}
func (r *MySQLUploads) CheckShareSchema(ctx context.Context) error {
	for _, query := range []string{`SELECT ` + shareColumns + ` FROM album_share_links LIMIT 0`, `SELECT ` + shareClaimColumns + ` FROM album_share_claims LIMIT 0`} {
		rows, err := r.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}
func (r *MySQLUploads) shareByRequest(ctx context.Context, owner, request string) (shareRow, error) {
	return scanShare(r.db.QueryRowContext(ctx, `SELECT `+shareColumns+` FROM album_share_links WHERE owner_id=? AND request_id=?`, owner, request))
}

// Lock ordering is account -> share -> claim. Signing is local and uses this
// transaction's immutable version, never a nested pool connection.
func (r *MySQLUploads) shareRetentionInTransaction(ctx context.Context, tx *sql.Tx, owner string, now time.Time) (time.Time, error) {
	if r.billingEnvironment != "Sandbox" && r.billingEnvironment != "Production" {
		return time.Time{}, ErrShareUnavailable
	}
	if _, err := r.lockAccount(ctx, tx, owner); err != nil {
		if errors.Is(err, ErrOwner) {
			return time.Time{}, ErrShareNotAvailable
		}
		return time.Time{}, err
	}
	var accountState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM album_accounts WHERE account_id=?`, owner).Scan(&accountState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, ErrShareNotAvailable
		}
		return time.Time{}, err
	}
	if accountState != "active" {
		return time.Time{}, ErrShareNotAvailable
	}
	a, err := r.cloudAccountInTransaction(ctx, tx, owner, now)
	if err != nil {
		return time.Time{}, err
	}
	if (a.State != "active" && a.State != "expired") || a.RetentionDeadline == nil || !now.Before(*a.RetentionDeadline) {
		return time.Time{}, ErrShareNotAvailable
	}
	return *a.RetentionDeadline, nil
}
func (r *MySQLUploads) createShare(ctx context.Context, candidate shareRow, now time.Time) (shareRow, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return shareRow{}, err
	}
	defer tx.Rollback()
	until, err := r.shareRetentionInTransaction(ctx, tx, candidate.Owner, now)
	if err != nil {
		return shareRow{}, err
	}
	if err := requireVisibleVersion(ctx, tx, candidate.Owner, candidate.Version); err != nil {
		return shareRow{}, ErrShareNotAvailable
	}
	old, err := scanShare(tx.QueryRowContext(ctx, `SELECT `+shareColumns+` FROM album_share_links WHERE owner_id=? AND request_id=? FOR UPDATE`, candidate.Owner, candidate.Request))
	if err == nil {
		return old, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return shareRow{}, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_asset_versions WHERE owner_id=? AND version_id=?`, candidate.Owner, candidate.Version).Scan(&exists); err != nil {
		return shareRow{}, err
	}
	if exists != 1 {
		return shareRow{}, ErrNotFound
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_share_links WHERE owner_id=? AND version_id=? AND revoked_at IS NULL AND expires_at>?`, candidate.Owner, candidate.Version, now.UTC()).Scan(&count); err != nil {
		return shareRow{}, err
	}
	if count >= shareMaxActiveLinks {
		return shareRow{}, ErrShareLinksLimit
	}
	candidate.Expiry = minTime(candidate.Expiry, until)
	if !now.Before(candidate.Expiry) {
		return shareRow{}, ErrShareNotAvailable
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO album_share_links(share_id,owner_id,version_id,request_id,key_id,request_digest,token_digest,password_proof,max_claims,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, candidate.ID, candidate.Owner, candidate.Version, candidate.Request, candidate.KeyID, candidate.RequestDigest[:], candidate.TokenDigest[:], candidate.Password, candidate.MaxClaims, candidate.Created.UTC(), candidate.Expiry.UTC())
	if err != nil {
		return shareRow{}, err
	}
	return candidate, tx.Commit()
}
func (r *MySQLUploads) ownerShares(ctx context.Context, owner, version string, now time.Time) ([]shareRow, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	until, err := r.shareRetentionInTransaction(ctx, tx, owner, now)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+shareColumns+` FROM album_share_links WHERE owner_id=? AND version_id=? AND revoked_at IS NULL AND expires_at>? ORDER BY created_at DESC,share_id LIMIT 32`, owner, version, now.UTC())
	if err != nil {
		return nil, err
	}
	result := []shareRow{}
	for rows.Next() {
		row, err := scanShare(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		row.Expiry = minTime(row.Expiry, until)
		result = append(result, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}
func (r *MySQLUploads) revokeShare(ctx context.Context, owner, id string, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := r.lockAccount(ctx, tx, owner); err != nil {
		return err
	}
	row, err := scanShare(tx.QueryRowContext(ctx, `SELECT `+shareColumns+` FROM album_share_links WHERE owner_id=? AND share_id=? FOR UPDATE`, owner, id))
	if err != nil {
		return err
	}
	if row.Revoked != nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_share_links SET revoked_at=? WHERE owner_id=? AND share_id=?`, now.UTC(), owner, id); err != nil {
		return err
	}
	return tx.Commit()
}
func validateActiveShare(row shareRow, digest [32]byte, now, retention time.Time) (shareRow, error) {
	if subtle.ConstantTimeCompare(row.TokenDigest[:], digest[:]) != 1 || row.Revoked != nil || !now.Before(row.Expiry) || !now.Before(retention) {
		return shareRow{}, ErrShareNotAvailable
	}
	row.Expiry = minTime(row.Expiry, retention)
	return row, nil
}
func (r *MySQLUploads) activeShare(ctx context.Context, id string, digest [32]byte, now time.Time) (shareRow, error) {
	initial, err := scanShare(r.db.QueryRowContext(ctx, `SELECT `+shareColumns+` FROM album_share_links WHERE share_id=?`, id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return shareRow{}, ErrShareNotAvailable
		}
		return shareRow{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return shareRow{}, err
	}
	defer tx.Rollback()
	until, err := r.shareRetentionInTransaction(ctx, tx, initial.Owner, now)
	if err != nil {
		return shareRow{}, err
	}
	row, err := scanShare(tx.QueryRowContext(ctx, `SELECT `+shareColumns+` FROM album_share_links WHERE share_id=? AND owner_id=? FOR UPDATE`, id, initial.Owner))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return shareRow{}, ErrShareNotAvailable
		}
		return shareRow{}, err
	}
	row, err = validateActiveShare(row, digest, now, until)
	if err != nil {
		return shareRow{}, err
	}
	return row, tx.Commit()
}
func (r *MySQLUploads) claimShare(ctx context.Context, snapshot shareRow, digest [32]byte, candidate shareClaimRow, now time.Time) (shareClaimRow, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return shareClaimRow{}, err
	}
	defer tx.Rollback()
	until, err := r.shareRetentionInTransaction(ctx, tx, snapshot.Owner, now)
	if err != nil {
		return shareClaimRow{}, err
	}
	row, err := scanShare(tx.QueryRowContext(ctx, `SELECT `+shareColumns+` FROM album_share_links WHERE share_id=? AND owner_id=? FOR UPDATE`, snapshot.ID, snapshot.Owner))
	if err != nil {
		return shareClaimRow{}, err
	}
	row, err = validateActiveShare(row, digest, now, until)
	if err != nil {
		return shareClaimRow{}, err
	}
	if !bytes.Equal(row.Password, snapshot.Password) {
		return shareClaimRow{}, ErrShareNotAvailable
	}
	previous, err := scanShareClaim(tx.QueryRowContext(ctx, `SELECT `+shareClaimColumns+` FROM album_share_claims WHERE share_id=? AND request_id=? FOR UPDATE`, row.ID, candidate.Request))
	if err == nil {
		if !now.Before(previous.Expiry) {
			return shareClaimRow{}, ErrShareClaimExpired
		}
		previous.Expiry = minTime(previous.Expiry, row.Expiry)
		return previous, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return shareClaimRow{}, err
	}
	if row.Claims >= row.MaxClaims {
		return shareClaimRow{}, ErrShareLimit
	}
	candidate.Expiry = minTime(candidate.Expiry, row.Expiry)
	if !now.Before(candidate.Expiry) {
		return shareClaimRow{}, ErrShareNotAvailable
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO album_share_claims(claim_id,share_id,request_id,key_id,token_digest,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`, candidate.ID, candidate.Share, candidate.Request, candidate.KeyID, candidate.TokenDigest[:], candidate.Created.UTC(), candidate.Expiry.UTC())
	if err != nil {
		return shareClaimRow{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_share_links SET claimed_count=claimed_count+1 WHERE share_id=?`, row.ID); err != nil {
		return shareClaimRow{}, err
	}
	return candidate, tx.Commit()
}
func (r *MySQLUploads) withShareClaim(ctx context.Context, id string, digest [32]byte, now time.Time, authorize func(shareRow, AssetVersion, time.Time) error) error {
	initial, err := scanShareClaim(r.db.QueryRowContext(ctx, `SELECT `+shareClaimColumns+` FROM album_share_claims WHERE claim_id=?`, id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrShareNotAvailable
		}
		return err
	}
	parent, err := scanShare(r.db.QueryRowContext(ctx, `SELECT `+shareColumns+` FROM album_share_links WHERE share_id=?`, initial.Share))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrShareNotAvailable
		}
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	until, err := r.shareRetentionInTransaction(ctx, tx, parent.Owner, now)
	if err != nil {
		return err
	}
	row, err := scanShare(tx.QueryRowContext(ctx, `SELECT `+shareColumns+` FROM album_share_links WHERE share_id=? AND owner_id=? FOR UPDATE`, parent.ID, parent.Owner))
	if err != nil {
		return err
	}
	// Claim authentication replaces link authentication; the original immutable
	// digest is used only to apply the shared lifecycle check.
	row, err = validateActiveShare(row, row.TokenDigest, now, until)
	if err != nil {
		return err
	}
	claim, err := scanShareClaim(tx.QueryRowContext(ctx, `SELECT `+shareClaimColumns+` FROM album_share_claims WHERE claim_id=? AND share_id=? FOR UPDATE`, id, row.ID))
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(claim.TokenDigest[:], digest[:]) != 1 {
		return ErrShareNotAvailable
	}
	if !now.Before(claim.Expiry) {
		return ErrShareClaimExpired
	}
	if err := requireVisibleVersion(ctx, tx, row.Owner, row.Version); err != nil {
		return ErrShareNotAvailable
	}
	version, err := scanAssetVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND version_id=?`, row.Owner, row.Version))
	if err != nil {
		return ErrShareNotAvailable
	}
	if err := validateReadableVersion(version, row.Owner, row.Version); err != nil {
		return ErrShareNotAvailable
	}
	if err := authorize(row, version, minTime(claim.Expiry, row.Expiry)); err != nil {
		return err
	}
	return tx.Commit()
}
