package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

func (r *MySQLUploads) PhotoWriteByAttempt(ctx context.Context, owner, attempt string) (PhotoWriteRecord, error) {
	if !validOwner(owner) || !validOwner(attempt) {
		return PhotoWriteRecord{}, ErrInvalidManifest
	}
	return scanPhotoWrite(r.db.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND attempt_id=?`, owner, attempt))
}

// Account-first locking fences every receipt, closure and retirement. Queue
// leases alone cannot authorize deleting a published or newly recovered output.
func (r *MySQLUploads) lockDerivedCleanup(ctx context.Context, tx *sql.Tx, expected DerivedCleanup, now time.Time) (DerivedCleanup, error) {
	if !validOwner(expected.OwnerID) || !validOwner(expected.ID) || now.IsZero() {
		return DerivedCleanup{}, ErrInvalidManifest
	}
	if _, err := lockAccount(ctx, tx, expected.OwnerID); err != nil {
		return DerivedCleanup{}, err
	}
	c, err := scanCleanup(tx.QueryRowContext(ctx, `SELECT `+cleanupColumns+` FROM album_derived_cleanups WHERE owner_id=? AND cleanup_id=? FOR UPDATE`, expected.OwnerID, expected.ID))
	if err != nil {
		return DerivedCleanup{}, err
	}
	if !cleanupLease(c, expected, now) {
		return DerivedCleanup{}, ErrLease
	}
	f, err := scanFinalization(tx.QueryRowContext(ctx, `SELECT `+finalizationColumns+` FROM album_derived_finalizations WHERE owner_id=? AND version_id=? FOR UPDATE`, c.OwnerID, c.VersionID))
	if err != nil {
		return DerivedCleanup{}, err
	}
	if f.ManifestDigest != c.ManifestDigest || f.HeldBytes < c.ReserveBytes {
		return DerivedCleanup{}, ErrConflict
	}
	return c, nil
}

func cleanupPhotoMatches(c DerivedCleanup, p PhotoWriteRecord) bool {
	return c.Kind == "photo" && p.Spec.OwnerID == c.OwnerID && p.Spec.AttemptID == c.ReferenceID && p.VersionID == c.VersionID &&
		p.ManifestDigest == c.ManifestDigest && p.ObjectVersion == c.KeepVersion &&
		((c.KeepVersion == "" && c.ReserveBytes == p.Spec.SizeBytes) || (c.KeepVersion != "" && c.ReserveBytes == 0))
}
func cleanupVideoMatches(c DerivedCleanup, j VideoJob) bool {
	if c.Kind != "video" || j.OwnerID != c.OwnerID || j.ID != c.ReferenceID || j.VersionID != c.VersionID || j.ManifestDigest != c.ManifestDigest || j.Spec == nil {
		return false
	}
	if c.KeepVersion == "" {
		return j.Result == nil && j.State == "attention" && c.ReserveBytes == j.Spec.MaxBytes
	}
	return j.Result != nil && j.State == "completed" && j.Result.Object.VersionID == c.KeepVersion && c.ReserveBytes == 0
}

func (r *MySQLUploads) StoreDerivedCleanupPlan(ctx context.Context, expected DerivedCleanup, plan DerivedCleanupPlan, now time.Time) (DerivedCleanup, error) {
	if validateCleanupPlan(plan) != nil {
		return DerivedCleanup{}, ErrConflict
	}
	data, err := json.Marshal(plan)
	if err != nil || len(data) > 262144 {
		return DerivedCleanup{}, ErrConflict
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return DerivedCleanup{}, err
	}
	defer tx.Rollback()
	c, err := r.lockDerivedCleanup(ctx, tx, expected, now)
	if err != nil {
		return DerivedCleanup{}, err
	}
	if c.Plan != nil {
		old, _ := json.Marshal(c.Plan)
		if string(old) != string(data) {
			return DerivedCleanup{}, ErrConflict
		}
		return c, tx.Commit()
	}
	if c.KeepVersion != "" {
		found := false
		for _, v := range plan.Versions {
			if v.VersionID == c.KeepVersion && !v.DeleteMarker {
				found = true
			}
		}
		if !found {
			return DerivedCleanup{}, ErrConflict
		}
	}
	switch c.Kind {
	case "photo":
		p, err := scanPhotoWrite(tx.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND attempt_id=? FOR UPDATE`, c.OwnerID, c.ReferenceID))
		if err != nil {
			return DerivedCleanup{}, err
		}
		if !cleanupPhotoMatches(c, p) || !p.RetiredAt.IsZero() {
			return DerivedCleanup{}, ErrConflict
		}
		if c.KeepVersion != "" {
			for _, object := range plan.Versions {
				if object.VersionID == c.KeepVersion && object.SizeBytes != p.Spec.SizeBytes {
					return DerivedCleanup{}, ErrConflict
				}
			}
		}
		if c.KeepVersion == "" {
			committed := false
			for _, object := range plan.Versions {
				if !object.DeleteMarker {
					committed = true
				}
			}
			if !committed {
				return DerivedCleanup{}, ErrConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE album_photo_writes SET retirement_started_at=?,updated_at=? WHERE owner_id=? AND attempt_id=? AND object_version IS NULL`, now, now, c.OwnerID, c.ReferenceID); err != nil {
				return DerivedCleanup{}, err
			}
		}
	case "video":
		j, err := scanVideoJob(tx.QueryRowContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND job_id=? FOR UPDATE`, c.OwnerID, c.ReferenceID))
		if err != nil {
			return DerivedCleanup{}, err
		}
		if !cleanupVideoMatches(c, j) || !j.RetiredAt.IsZero() || !ciJobPattern.MatchString(j.ProviderJobID) {
			return DerivedCleanup{}, ErrConflict
		}
		if c.KeepVersion != "" {
			for _, object := range plan.Versions {
				if object.VersionID == c.KeepVersion && object.SizeBytes != j.Result.Object.SizeBytes {
					return DerivedCleanup{}, ErrConflict
				}
			}
		}
		if c.KeepVersion == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE album_video_jobs SET retirement_started_at=?,updated_at=? WHERE owner_id=? AND job_id=? AND result_json IS NULL`, now, now, c.OwnerID, c.ReferenceID); err != nil {
				return DerivedCleanup{}, err
			}
		}
	default:
		return DerivedCleanup{}, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_derived_cleanups SET plan_json=?,updated_at=? WHERE cleanup_id=?`, data, now, c.ID); err != nil {
		return DerivedCleanup{}, err
	}
	c.Plan = &plan
	return c, tx.Commit()
}

// The worker calls this only after a new complete version listing proves every
// planned disposable version absent and the retained receipt (if any) present.
func (r *MySQLUploads) FinishDerivedRetirement(ctx context.Context, expected DerivedCleanup, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := r.lockDerivedCleanup(ctx, tx, expected, now)
	if err != nil {
		return err
	}
	if c.Plan == nil || !c.Plan.Terminal {
		return ErrConflict
	}
	if c.KeepVersion == "" {
		switch c.Kind {
		case "photo":
			p, err := scanPhotoWrite(tx.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND attempt_id=? FOR UPDATE`, c.OwnerID, c.ReferenceID))
			if err != nil {
				return err
			}
			if !cleanupPhotoMatches(c, p) || p.RetirementStartedAt.IsZero() || !p.RetiredAt.IsZero() {
				return ErrConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE album_photo_writes SET retired_at=?,updated_at=? WHERE owner_id=? AND attempt_id=?`, now, now, c.OwnerID, c.ReferenceID); err != nil {
				return err
			}
		case "video":
			j, err := scanVideoJob(tx.QueryRowContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND job_id=? FOR UPDATE`, c.OwnerID, c.ReferenceID))
			if err != nil {
				return err
			}
			if !cleanupVideoMatches(c, j) || j.RetirementStartedAt.IsZero() || !j.RetiredAt.IsZero() {
				return ErrConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE album_video_jobs SET retired_at=?,updated_at=? WHERE owner_id=? AND job_id=?`, now, now, c.OwnerID, c.ReferenceID); err != nil {
				return err
			}
		default:
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx, `UPDATE album_storage_accounts SET reserved_bytes=reserved_bytes-? WHERE owner_id=? AND reserved_bytes>=?`, c.ReserveBytes, c.OwnerID, c.ReserveBytes)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrQuota
		}
		if _, err := tx.ExecContext(ctx, `UPDATE album_derived_finalizations SET held_bytes=held_bytes-?,released_bytes=released_bytes+?,updated_at=? WHERE owner_id=? AND version_id=?`, c.ReserveBytes, c.ReserveBytes, now, c.OwnerID, c.VersionID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_derived_cleanups SET state='completed',lease_token=NULL,lease_until=NULL,updated_at=? WHERE cleanup_id=?`, now, c.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *MySQLUploads) BindCleanupVideoSubmission(ctx context.Context, expected DerivedCleanup, id string, now time.Time) (VideoJob, error) {
	if !ciJobPattern.MatchString(id) {
		return VideoJob{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return VideoJob{}, err
	}
	defer tx.Rollback()
	c, err := r.lockDerivedCleanup(ctx, tx, expected, now)
	if err != nil {
		return VideoJob{}, err
	}
	j, err := scanVideoJob(tx.QueryRowContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND job_id=? FOR UPDATE`, c.OwnerID, c.ReferenceID))
	if err != nil {
		return VideoJob{}, err
	}
	if !cleanupVideoMatches(c, j) || (j.ProviderJobID != "" && j.ProviderJobID != id) || !j.RetirementStartedAt.IsZero() {
		return VideoJob{}, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_video_jobs SET provider_job_id=?,updated_at=? WHERE job_id=?`, id, now, j.ID); err != nil {
		return VideoJob{}, err
	}
	j.ProviderJobID = id
	return j, tx.Commit()
}

func (r *MySQLUploads) RecoverCleanupVideo(ctx context.Context, expected DerivedCleanup, result VideoResult, now time.Time) (VideoJob, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return VideoJob{}, err
	}
	defer tx.Rollback()
	c, err := r.lockDerivedCleanup(ctx, tx, expected, now)
	if err != nil {
		return VideoJob{}, err
	}
	j, err := scanVideoJob(tx.QueryRowContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND job_id=? FOR UPDATE`, c.OwnerID, c.ReferenceID))
	if err != nil {
		return VideoJob{}, err
	}
	if !cleanupVideoMatches(c, j) || c.KeepVersion != "" || !j.RetirementStartedAt.IsZero() || !ciJobPattern.MatchString(j.ProviderJobID) || j.validateResult(result) != nil {
		return VideoJob{}, ErrConflict
	}
	j, err = publishVideoResult(ctx, tx, j, result, now)
	if err != nil {
		return VideoJob{}, err
	}
	return j, tx.Commit()
}
