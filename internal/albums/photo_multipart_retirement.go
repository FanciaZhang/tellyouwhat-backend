package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

func (r *MySQLUploads) lockCleanupPhotoMultipart(ctx context.Context, tx *sql.Tx, c DerivedCleanup, expected PhotoWriteRecord, now time.Time) (PhotoWriteRecord, error) {
	c, err := r.lockDerivedCleanup(ctx, tx, c, now)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	p, err := scanPhotoWrite(tx.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND attempt_id=? FOR UPDATE`, c.OwnerID, c.ReferenceID))
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	if c.Plan != nil || c.KeepVersion != "" || !cleanupPhotoMatches(c, p) || !p.Spec.Multipart || !validCOSMultipartID(p.MultipartID) || !p.RetiredAt.IsZero() ||
		p.Spec != expected.Spec || p.VersionID != expected.VersionID || p.ManifestDigest != expected.ManifestDigest || p.MultipartID != expected.MultipartID {
		return PhotoWriteRecord{}, ErrConflict
	}
	return p, nil
}

// The retirement fence wins atomically against PreparePhotoMultipartCompletion.
func (r *MySQLUploads) FenceCleanupPhotoMultipart(ctx context.Context, c DerivedCleanup, expected PhotoWriteRecord, now time.Time) (PhotoWriteRecord, error) {
	now = now.UTC().Truncate(time.Microsecond)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	defer tx.Rollback()
	p, err := r.lockCleanupPhotoMultipart(ctx, tx, c, expected, now)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	if p.MultipartPhase != "ready" && p.MultipartPhase != "aborted" {
		return PhotoWriteRecord{}, ErrDerivedPending
	}
	if p.RetirementStartedAt.IsZero() {
		if _, err := tx.ExecContext(ctx, `UPDATE album_photo_writes SET retirement_started_at=?,updated_at=? WHERE owner_id=? AND attempt_id=?`, now, now, p.Spec.OwnerID, p.Spec.AttemptID); err != nil {
			return PhotoWriteRecord{}, err
		}
		p.RetirementStartedAt = now
	}
	return p, tx.Commit()
}

func (r *MySQLUploads) ConfirmCleanupPhotoMultipartAborted(ctx context.Context, c DerivedCleanup, expected PhotoWriteRecord, now time.Time) (PhotoWriteRecord, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	defer tx.Rollback()
	p, err := r.lockCleanupPhotoMultipart(ctx, tx, c, expected, now)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	if p.RetirementStartedAt.IsZero() || expected.RetirementStartedAt.IsZero() || !p.RetirementStartedAt.Equal(expected.RetirementStartedAt) ||
		(p.MultipartPhase != "ready" && p.MultipartPhase != "aborted") {
		return PhotoWriteRecord{}, ErrConflict
	}
	if p.MultipartPhase != "aborted" {
		if _, err := tx.ExecContext(ctx, `UPDATE album_photo_writes SET multipart_phase='aborted',updated_at=? WHERE owner_id=? AND attempt_id=?`, now, p.Spec.OwnerID, p.Spec.AttemptID); err != nil {
			return PhotoWriteRecord{}, err
		}
		p.MultipartPhase = "aborted"
	}
	return p, tx.Commit()
}

// A missing session is conclusive only while the durable fence proves Complete
// was never admitted. No object version is deleted by this operation.
func (s *COSObjects) AbortPhotoMultipart(ctx context.Context, p PhotoWriteRecord) error {
	key, err := p.Spec.key()
	if err != nil || !p.Spec.Multipart || !validCOSMultipartID(p.MultipartID) || p.MultipartPhase != "ready" || p.ObjectVersion != "" || p.RetirementStartedAt.IsZero() || !p.RetiredAt.IsZero() {
		return ErrInvalidManifest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.abortPurgeUpload(ctx, p.Spec.OwnerID, purgeTarget{key}, p.MultipartID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, response, err := s.client.Object.ListParts(ctx, key, p.MultipartID, nil)
	closePhotoResponse(response)
	var provider *cos.ErrorResponse
	if response == nil || response.StatusCode != http.StatusNotFound || !errors.As(err, &provider) || provider.Code != "NoSuchUpload" {
		return ErrPhotoWriteUncertain
	}
	return nil
}
