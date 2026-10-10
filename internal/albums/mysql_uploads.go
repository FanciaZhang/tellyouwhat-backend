package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"
)

type MySQLUploads struct{ db *sql.DB }

func NewMySQLUploads(db *sql.DB) *MySQLUploads { return &MySQLUploads{db: db} }

// ProvisionAccount is an internal account/billing operation, never a client quota setter.
// An existing account's quota is not replaced by a replay of signup provisioning.
func (r *MySQLUploads) ProvisionAccount(ctx context.Context, owner string, quota int64) error {
	if !validOwner(owner) || quota < 0 {
		return ErrOwner
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO album_storage_accounts(owner_id, quota_bytes) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE owner_id = VALUES(owner_id)`, owner, quota)
	return err
}

type quotaBalance struct{ limit, used, reserved int64 }

func lockAccount(ctx context.Context, tx *sql.Tx, owner string) (quotaBalance, error) {
	var b quotaBalance
	err := tx.QueryRowContext(ctx, `SELECT quota_bytes, used_bytes, reserved_bytes FROM album_storage_accounts WHERE owner_id=? FOR UPDATE`, owner).Scan(&b.limit, &b.used, &b.reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrOwner
	}
	return b, err
}

const uploadColumns = `upload_id, owner_id, request_id, manifest_json, manifest_digest, state, original_bytes, derived_budget, expires_at, lease_token, lease_until, sealed_objects`

type rowScanner interface{ Scan(...any) error }

func scanUpload(row rowScanner) (Upload, error) {
	var u Upload
	var manifest, objects []byte
	var lease sql.NullString
	var until sql.NullTime
	err := row.Scan(&u.ID, &u.OwnerID, &u.RequestID, &manifest, &u.Digest, &u.State, &u.OriginalBytes, &u.DerivedBudget, &u.ExpiresAt, &lease, &until, &objects)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	if err := json.Unmarshal(manifest, &u.Manifest); err != nil {
		return u, err
	}
	if len(objects) > 0 {
		if err := json.Unmarshal(objects, &u.Objects); err != nil {
			return u, err
		}
	}
	u.LeaseToken = lease.String
	u.LeaseUntil = until.Time
	return u, nil
}

func (r *MySQLUploads) Reserve(ctx context.Context, u Upload) (Upload, error) {
	digest, err := u.Manifest.Digest()
	if err != nil || digest != u.Digest || !validOwner(u.OwnerID) || !validOwner(u.ID) || !validOwner(u.RequestID) ||
		u.OriginalBytes <= 0 || u.DerivedBudget <= 0 || u.OriginalBytes > math.MaxInt64-u.DerivedBudget {
		return Upload{}, ErrInvalidManifest
	}
	var total int64
	for _, resource := range u.Manifest.Resources {
		total += resource.SizeBytes
	}
	if total != u.OriginalBytes || u.State != Uploading {
		return Upload{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Upload{}, err
	}
	defer tx.Rollback()
	account, err := lockAccount(ctx, tx, u.OwnerID)
	if err != nil {
		return Upload{}, err
	}
	old, err := scanUpload(tx.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM album_uploads WHERE owner_id=? AND request_id=? FOR UPDATE`, u.OwnerID, u.RequestID))
	if err == nil {
		if old.Digest != u.Digest {
			return Upload{}, ErrConflict
		}
		return old, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return Upload{}, err
	}
	old, err = scanUpload(tx.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM album_uploads WHERE owner_id=? AND manifest_digest=? FOR UPDATE`, u.OwnerID, u.Digest))
	if err == nil {
		return old, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return Upload{}, err
	}
	reserve := u.OriginalBytes + u.DerivedBudget
	if account.used > account.limit || account.reserved > account.limit-account.used || reserve > account.limit-account.used-account.reserved {
		return Upload{}, ErrQuota
	}
	manifest, err := json.Marshal(u.Manifest)
	if err != nil {
		return Upload{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO album_uploads(upload_id,owner_id,request_id,manifest_json,manifest_digest,state,original_bytes,derived_budget,expires_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, u.ID, u.OwnerID, u.RequestID, manifest, u.Digest, u.State, u.OriginalBytes, u.DerivedBudget, u.ExpiresAt)
	if err != nil {
		return Upload{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE album_storage_accounts SET reserved_bytes=reserved_bytes+? WHERE owner_id=?`, reserve, u.OwnerID)
	if err != nil {
		return Upload{}, err
	}
	return u, tx.Commit()
}

func (r *MySQLUploads) Get(ctx context.Context, owner, id string) (Upload, error) {
	return scanUpload(r.db.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM album_uploads WHERE owner_id=? AND upload_id=?`, owner, id))
}

func (r *MySQLUploads) mutate(ctx context.Context, owner, id string, action func(*sql.Tx, Upload) (Upload, error)) (Upload, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Upload{}, err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, owner); err != nil {
		return Upload{}, err
	}
	upload, err := scanUpload(tx.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM album_uploads WHERE owner_id=? AND upload_id=? FOR UPDATE`, owner, id))
	if err != nil {
		return Upload{}, err
	}
	result, err := action(tx, upload)
	if err != nil {
		return Upload{}, err
	}
	return result, tx.Commit()
}

func (r *MySQLUploads) Submit(ctx context.Context, owner, id string, now time.Time) (Upload, error) {
	return r.mutate(ctx, owner, id, func(tx *sql.Tx, u Upload) (Upload, error) {
		if u.State == OriginalsVerified || u.State == Queued || u.State == Verifying {
			return u, nil
		}
		if !now.Before(u.ExpiresAt) {
			return Upload{}, ErrExpired
		}
		if u.State != Uploading {
			return Upload{}, ErrConflict
		}
		_, err := tx.ExecContext(ctx, `UPDATE album_uploads SET state='queued',updated_at=? WHERE owner_id=? AND upload_id=?`, now, owner, id)
		u.State = Queued
		return u, err
	})
}

func (r *MySQLUploads) Claim(ctx context.Context, owner, id, token string, now, until time.Time) (Upload, error) {
	if !validOwner(token) || !until.After(now) {
		return Upload{}, ErrLease
	}
	return r.mutate(ctx, owner, id, func(tx *sql.Tx, u Upload) (Upload, error) {
		if u.State == OriginalsVerified {
			return u, nil
		}
		if !now.Before(u.ExpiresAt) {
			return Upload{}, ErrExpired
		}
		if u.State != Queued && !(u.State == Verifying && !now.Before(u.LeaseUntil)) {
			return Upload{}, ErrLease
		}
		_, err := tx.ExecContext(ctx, `UPDATE album_uploads SET state='verifying',lease_token=?,lease_until=?,updated_at=? WHERE owner_id=? AND upload_id=?`, token, until, now, owner, id)
		u.State = Verifying
		u.LeaseToken = token
		u.LeaseUntil = until
		return u, err
	})
}

func (r *MySQLUploads) Complete(ctx context.Context, owner, id, token string, objects []SealedObject, now time.Time) (Upload, error) {
	return r.mutate(ctx, owner, id, func(tx *sql.Tx, u Upload) (Upload, error) {
		if u.State == OriginalsVerified {
			return u, nil
		}
		if u.State != Verifying || u.LeaseToken != token || !now.Before(u.LeaseUntil) {
			return Upload{}, ErrLease
		}
		observations := make([]Observation, 0, len(objects))
		for _, object := range objects {
			if object.Key != sealedKey(u, object.ResourceID) || object.VersionID == "" {
				return Upload{}, ErrIncompleteBackup
			}
			observations = append(observations, Observation{object.ResourceID, object.SizeBytes, object.SHA256})
		}
		if digest, err := VerifyBackup(u.Manifest, observations); err != nil || digest != u.Digest {
			return Upload{}, ErrIncompleteBackup
		}
		encoded, err := json.Marshal(objects)
		if err != nil {
			return Upload{}, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=used_bytes+?,reserved_bytes=reserved_bytes-? WHERE owner_id=? AND reserved_bytes>=?`, u.OriginalBytes, u.OriginalBytes, owner, u.OriginalBytes)
		if err != nil {
			return Upload{}, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return Upload{}, err
		}
		if count != 1 {
			return Upload{}, ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE album_uploads SET state='originals_verified',sealed_objects=?,lease_token=NULL,lease_until=NULL,updated_at=? WHERE owner_id=? AND upload_id=?`, encoded, now, owner, id)
		u.State = OriginalsVerified
		u.Objects = objects
		u.LeaseToken = ""
		u.LeaseUntil = time.Time{}
		return u, err
	})
}

// Pending is a trusted-worker inventory; account-scoped HTTP endpoints never call it.
func (r *MySQLUploads) Pending(ctx context.Context, now time.Time, limit int) ([]Upload, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrConflict
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+uploadColumns+` FROM album_uploads WHERE expires_at>? AND
		(state='queued' OR (state='verifying' AND lease_until<=?)) ORDER BY created_at,upload_id LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, u)
	}
	return result, rows.Err()
}
