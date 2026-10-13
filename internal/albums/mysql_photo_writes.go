package albums

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// An empty ObjectVersion means the attempt is consumed, with no verified receipt.
// It never authorizes another PUT, including after a process restart.
type PhotoWriteRecord struct {
	VersionID                      string
	ManifestDigest                 string
	Spec                           PhotoWriteSpec
	ObjectVersion                  string
	RetirementStartedAt, RetiredAt time.Time
}

func (r PhotoWriteRecord) Receipt() (SealedObject, error) {
	key, err := r.Spec.key()
	if err != nil || !concreteCOSVersion(r.ObjectVersion) || !r.RetirementStartedAt.IsZero() || !r.RetiredAt.IsZero() {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	return SealedObject{ResourceID: string(r.Spec.Preset), Key: key, VersionID: r.ObjectVersion, SizeBytes: r.Spec.SizeBytes, SHA256: r.Spec.SHA256}, nil
}

const photoWriteColumns = `version_id,manifest_digest,owner_id,attempt_id,preset,size_bytes,sha256,pixel_width,pixel_height,object_version,retirement_started_at,retired_at`

func scanPhotoWrite(row rowScanner) (PhotoWriteRecord, error) {
	var r PhotoWriteRecord
	var version sql.NullString
	var retiring, retired sql.NullTime
	err := row.Scan(&r.VersionID, &r.ManifestDigest, &r.Spec.OwnerID, &r.Spec.AttemptID, &r.Spec.Preset, &r.Spec.SizeBytes, &r.Spec.SHA256, &r.Spec.Width, &r.Spec.Height, &version, &retiring, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.ObjectVersion = version.String
	r.RetirementStartedAt, r.RetiredAt = retiring.Time, retired.Time
	digest, err := hex.DecodeString(r.ManifestDigest)
	if _, specErr := r.Spec.key(); specErr != nil || !validOwner(r.VersionID) || err != nil || len(digest) != 32 || (version.Valid && !concreteCOSVersion(version.String)) ||
		(retired.Valid && !retiring.Valid) || (version.Valid && retiring.Valid) {
		return PhotoWriteRecord{}, ErrConflict
	}
	return r, nil
}
func (r *MySQLUploads) PhotoWrite(ctx context.Context, owner, versionID string, preset PhotoPreset) (PhotoWriteRecord, error) {
	if !validOwner(owner) || !validOwner(versionID) {
		return PhotoWriteRecord{}, ErrInvalidManifest
	}
	return scanPhotoWrite(r.db.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND version_id=? AND preset=?`, owner, versionID, preset))
}

// BeginPhotoWrite consumes this attempt in the database BEFORE its PUT. Only
// the caller receiving fresh=true after a successful commit may issue the PUT.
// An ambiguous DB commit returns no permission to write; reread and reconcile.
func (r *MySQLUploads) BeginPhotoWrite(ctx context.Context, versionID, digest string, spec PhotoWriteSpec, now time.Time) (PhotoWriteRecord, bool, error) {
	if _, err := spec.key(); err != nil || !validOwner(versionID) {
		return PhotoWriteRecord{}, false, ErrInvalidManifest
	}
	var record PhotoWriteRecord
	fresh := false
	_, err := r.mutate(ctx, spec.OwnerID, versionID, func(tx *sql.Tx, u Upload) (Upload, error) {
		if u.State != OriginalsVerified || u.Digest != digest {
			return u, ErrIncompleteBackup
		}
		old, err := scanPhotoWrite(tx.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND version_id=? AND preset=? FOR UPDATE`, spec.OwnerID, versionID, spec.Preset))
		if err == nil {
			if old.ManifestDigest != digest {
				return u, ErrConflict
			}
			record = old
			return u, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return u, err
		}
		if err := requireVisibleVersion(ctx, tx, u.OwnerID, u.ID); err != nil {
			return u, err
		}
		if err := derivedBudgetOpen(ctx, tx, spec.OwnerID, versionID); err != nil {
			return u, err
		}
		var allocated int64
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COALESCE(SUM(size_bytes),0) FROM album_photo_writes WHERE owner_id=? AND version_id=?)+(SELECT COALESCE(SUM(reserve_size),0) FROM album_video_jobs WHERE owner_id=? AND version_id=?)`, spec.OwnerID, versionID, spec.OwnerID, versionID).Scan(&allocated); err != nil {
			return u, err
		}
		if allocated < 0 || allocated > u.DerivedBudget || spec.SizeBytes > u.DerivedBudget-allocated {
			return u, ErrQuota
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO album_photo_writes(attempt_id,owner_id,version_id,manifest_digest,preset,size_bytes,sha256,pixel_width,pixel_height,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, spec.AttemptID, spec.OwnerID, versionID, digest, spec.Preset, spec.SizeBytes, spec.SHA256, spec.Width, spec.Height, now, now)
		if err != nil {
			return u, err
		}
		record = PhotoWriteRecord{VersionID: versionID, ManifestDigest: digest, Spec: spec}
		fresh = true
		return u, nil
	})
	if err != nil {
		return PhotoWriteRecord{}, false, err
	}
	return record, fresh, nil
}

// CompletePhotoWrite atomically publishes the receipt and transfers its actual
// bytes from the upload's existing reserved budget into account used bytes.
// Closed budgets also settle this attempt's escrow in the same transaction.
func (r *MySQLUploads) CompletePhotoWrite(ctx context.Context, expected PhotoWriteRecord, receipt SealedObject, now time.Time) (PhotoWriteRecord, error) {
	key, err := expected.Spec.key()
	if err != nil || !validOwner(expected.VersionID) || receipt.Key != key || receipt.ResourceID != string(expected.Spec.Preset) || receipt.SizeBytes != expected.Spec.SizeBytes || receipt.SHA256 != expected.Spec.SHA256 || !concreteCOSVersion(receipt.VersionID) {
		return PhotoWriteRecord{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	defer tx.Rollback()
	if _, err = r.lockAccount(ctx, tx, expected.Spec.OwnerID); err != nil {
		return PhotoWriteRecord{}, err
	}
	old, err := scanPhotoWrite(tx.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND version_id=? AND preset=? FOR UPDATE`, expected.Spec.OwnerID, expected.VersionID, expected.Spec.Preset))
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	if old.Spec != expected.Spec || old.ManifestDigest != expected.ManifestDigest {
		return PhotoWriteRecord{}, ErrConflict
	}
	var observed sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT deletion_object_version FROM album_photo_writes WHERE attempt_id=?`, old.Spec.AttemptID).Scan(&observed); err != nil {
		return PhotoWriteRecord{}, err
	}
	if observed.Valid && observed.String != receipt.VersionID {
		return PhotoWriteRecord{}, ErrConflict
	}
	if !old.RetirementStartedAt.IsZero() || !old.RetiredAt.IsZero() {
		return PhotoWriteRecord{}, ErrDerivedRetired
	}
	if old.ObjectVersion != "" {
		if old.ObjectVersion != receipt.VersionID {
			return PhotoWriteRecord{}, ErrConflict
		}
		return old, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=used_bytes+?,reserved_bytes=reserved_bytes-? WHERE owner_id=? AND reserved_bytes>=?`, old.Spec.SizeBytes, old.Spec.SizeBytes, old.Spec.OwnerID, old.Spec.SizeBytes)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	if count != 1 {
		return PhotoWriteRecord{}, ErrQuota
	}
	if err := settleClosedDerivedWrite(ctx, tx, old.Spec.OwnerID, old.VersionID, old.ManifestDigest, "photo", old.Spec.AttemptID,
		old.Spec.SizeBytes, old.Spec.SizeBytes, receipt.VersionID, now); err != nil {
		return PhotoWriteRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE album_photo_writes SET object_version=?,updated_at=? WHERE attempt_id=?`, receipt.VersionID, now, old.Spec.AttemptID)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	old.ObjectVersion = receipt.VersionID
	return old, tx.Commit()
}
