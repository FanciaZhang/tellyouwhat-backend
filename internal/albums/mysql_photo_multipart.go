package albums

import (
	"context"
	"time"
)

// Persist before the provider receives Complete. Retirement and completion use
// the same account/record locks; an unknown historical phase is never abortable.
func (r *MySQLUploads) PreparePhotoMultipartCompletion(ctx context.Context, expected PhotoWriteRecord, now time.Time) (PhotoWriteRecord, error) {
	if _, err := expected.Spec.key(); err != nil || !expected.Spec.Multipart || !validCOSMultipartID(expected.MultipartID) || now.IsZero() {
		return PhotoWriteRecord{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	defer tx.Rollback()
	if _, err := r.lockAccount(ctx, tx, expected.Spec.OwnerID); err != nil {
		return PhotoWriteRecord{}, err
	}
	old, err := scanPhotoWrite(tx.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND attempt_id=? FOR UPDATE`, expected.Spec.OwnerID, expected.Spec.AttemptID))
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	if old.Spec != expected.Spec || old.VersionID != expected.VersionID || old.ManifestDigest != expected.ManifestDigest || old.MultipartID != expected.MultipartID {
		return PhotoWriteRecord{}, ErrConflict
	}
	if !old.RetirementStartedAt.IsZero() || !old.RetiredAt.IsZero() || old.MultipartPhase == "aborted" {
		return PhotoWriteRecord{}, ErrDerivedRetired
	}
	var deleting bool
	if err := tx.QueryRowContext(ctx, `SELECT deletion_requested_at IS NOT NULL FROM album_storage_accounts WHERE owner_id=?`, old.Spec.OwnerID).Scan(&deleting); err != nil {
		return PhotoWriteRecord{}, err
	}
	if deleting {
		return PhotoWriteRecord{}, ErrDerivedRetired
	}
	if err := requireVisibleVersion(ctx, tx, old.Spec.OwnerID, old.VersionID); err != nil {
		return PhotoWriteRecord{}, err
	}
	if old.MultipartPhase != "completing" {
		if _, err := tx.ExecContext(ctx, `UPDATE album_photo_writes SET multipart_phase='completing',updated_at=? WHERE owner_id=? AND attempt_id=?`, now, old.Spec.OwnerID, old.Spec.AttemptID); err != nil {
			return PhotoWriteRecord{}, err
		}
		old.MultipartPhase = "completing"
	}
	return old, tx.Commit()
}

// Binding records a provider observation, including after cancellation. It does
// not grant another initiation or allow replacing an already known session.
func (r *MySQLUploads) BindPhotoMultipart(ctx context.Context, expected PhotoWriteRecord, id string, now time.Time) (PhotoWriteRecord, error) {
	if _, err := expected.Spec.key(); err != nil || !expected.Spec.Multipart || !validCOSMultipartID(id) || !validOwner(expected.VersionID) || now.IsZero() {
		return PhotoWriteRecord{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, expected.Spec.OwnerID); err != nil {
		return PhotoWriteRecord{}, err
	}
	old, err := scanPhotoWrite(tx.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND attempt_id=? FOR UPDATE`, expected.Spec.OwnerID, expected.Spec.AttemptID))
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	if old.Spec != expected.Spec || old.VersionID != expected.VersionID || old.ManifestDigest != expected.ManifestDigest || old.MultipartID != "" && old.MultipartID != id {
		return PhotoWriteRecord{}, ErrConflict
	}
	if old.MultipartID == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE album_photo_writes SET provider_upload_id=?,updated_at=? WHERE owner_id=? AND attempt_id=?`, id, now, old.Spec.OwnerID, old.Spec.AttemptID); err != nil {
			return PhotoWriteRecord{}, err
		}
		old.MultipartID = id
	}
	return old, tx.Commit()
}
