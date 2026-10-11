package albums

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
)

var (
	ErrDerivedPending = errors.New("album derivative processing is not terminal")
	ErrDerivedClosed  = errors.New("album derivative budget is closed")
	ErrDerivedRetired = errors.New("album derivative is being retired")
)

type DerivedVersionRef struct{ OwnerID, VersionID string }
type DerivedFinalization struct {
	OwnerID, VersionID, ManifestDigest                   string
	BudgetBytes, RetainedBytes, HeldBytes, ReleasedBytes int64
	ClosedAt                                             time.Time
}

const finalizationColumns = `owner_id,version_id,manifest_digest,budget_bytes,retained_bytes,held_bytes,released_bytes,closed_at`

func scanFinalization(row rowScanner) (DerivedFinalization, error) {
	var f DerivedFinalization
	err := row.Scan(&f.OwnerID, &f.VersionID, &f.ManifestDigest, &f.BudgetBytes, &f.RetainedBytes, &f.HeldBytes, &f.ReleasedBytes, &f.ClosedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	if err != nil {
		return f, err
	}
	if !validOwner(f.OwnerID) || !validOwner(f.VersionID) || !validSHA256(f.ManifestDigest) || f.BudgetBytes <= 0 ||
		f.RetainedBytes < 0 || f.HeldBytes < 0 || f.ReleasedBytes < 0 || f.RetainedBytes > f.BudgetBytes ||
		f.HeldBytes > f.BudgetBytes-f.RetainedBytes || f.ReleasedBytes != f.BudgetBytes-f.RetainedBytes-f.HeldBytes || f.ClosedAt.IsZero() {
		return DerivedFinalization{}, ErrConflict
	}
	return f, nil
}
func (r *MySQLUploads) DerivedFinalization(ctx context.Context, owner, version string) (DerivedFinalization, error) {
	if !validOwner(owner) || !validOwner(version) {
		return DerivedFinalization{}, ErrInvalidManifest
	}
	return scanFinalization(r.db.QueryRowContext(ctx, `SELECT `+finalizationColumns+` FROM album_derived_finalizations WHERE owner_id=? AND version_id=?`, owner, version))
}
func derivedBudgetOpen(ctx context.Context, tx *sql.Tx, owner, version string) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM album_derived_finalizations WHERE owner_id=? AND version_id=?`, owner, version).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrDerivedClosed
}
func (r *MySQLUploads) PendingDerivedFinalizations(ctx context.Context, limit int) ([]DerivedVersionRef, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT u.owner_id,u.upload_id FROM album_uploads u
		WHERE u.state='originals_verified' AND u.derived_budget_closed=FALSE AND NOT EXISTS (SELECT 1 FROM album_derived_finalizations f WHERE f.owner_id=u.owner_id AND f.version_id=u.upload_id)
		AND (((SELECT COUNT(*) FROM album_photo_jobs p WHERE p.owner_id=u.owner_id AND p.version_id=u.upload_id)=2 AND (SELECT COUNT(*) FROM album_video_jobs v WHERE v.owner_id=u.owner_id AND v.version_id=u.upload_id)<=1)
		OR ((SELECT COUNT(*) FROM album_video_jobs v WHERE v.owner_id=u.owner_id AND v.version_id=u.upload_id)=1 AND NOT EXISTS (SELECT 1 FROM album_photo_jobs p WHERE p.owner_id=u.owner_id AND p.version_id=u.upload_id)))
		AND NOT EXISTS (SELECT 1 FROM album_photo_jobs p WHERE p.owner_id=u.owner_id AND p.version_id=u.upload_id AND p.state IN ('queued','running'))
		AND NOT EXISTS (SELECT 1 FROM album_video_jobs v WHERE v.owner_id=u.owner_id AND v.version_id=u.upload_id AND v.state IN ('queued','running'))
		ORDER BY u.updated_at,u.upload_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []DerivedVersionRef
	for rows.Next() {
		var ref DerivedVersionRef
		if err := rows.Scan(&ref.OwnerID, &ref.VersionID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// Account-first locking serializes closing with every paid intent and receipt.
// Terminal but uncertain attempts keep their exact bounded escrow; closing is
// never proof that a provider mutation did not happen.
func (r *MySQLUploads) FinalizeDerivedBudget(ctx context.Context, ref DerivedVersionRef, now time.Time) (DerivedFinalization, error) {
	if !validOwner(ref.OwnerID) || !validOwner(ref.VersionID) || now.IsZero() {
		return DerivedFinalization{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return DerivedFinalization{}, err
	}
	defer tx.Rollback()
	account, err := lockAccount(ctx, tx, ref.OwnerID)
	if err != nil {
		return DerivedFinalization{}, err
	}
	u, err := scanUpload(tx.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM album_uploads WHERE owner_id=? AND upload_id=? FOR UPDATE`, ref.OwnerID, ref.VersionID))
	if err != nil {
		return DerivedFinalization{}, err
	}
	if u.State != OriginalsVerified {
		return DerivedFinalization{}, ErrIncompleteBackup
	}
	old, err := scanFinalization(tx.QueryRowContext(ctx, `SELECT `+finalizationColumns+` FROM album_derived_finalizations WHERE owner_id=? AND version_id=? FOR UPDATE`, ref.OwnerID, ref.VersionID))
	if err == nil {
		if old.ManifestDigest != u.Digest || old.BudgetBytes != u.DerivedBudget {
			return DerivedFinalization{}, ErrConflict
		}
		return old, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return DerivedFinalization{}, err
	}
	photos, videos, err := terminalDerivedInventory(ctx, tx, u)
	if err != nil {
		return DerivedFinalization{}, err
	}
	f := DerivedFinalization{OwnerID: u.OwnerID, VersionID: u.ID, ManifestDigest: u.Digest, BudgetBytes: u.DerivedBudget, ClosedAt: now}
	type cleanup struct {
		kind, ref, keep string
		reserve         int64
	}
	var tasks []cleanup
	add := func(value int64, held bool) error {
		if value < 0 || value > math.MaxInt64-f.RetainedBytes-f.HeldBytes {
			return ErrConflict
		}
		if held {
			f.HeldBytes += value
		} else {
			f.RetainedBytes += value
		}
		if f.RetainedBytes > f.BudgetBytes || f.HeldBytes > f.BudgetBytes-f.RetainedBytes {
			return ErrQuota
		}
		return nil
	}
	for _, p := range photos {
		pending := p.ObjectVersion == ""
		if err := add(p.Spec.SizeBytes, pending); err != nil {
			return DerivedFinalization{}, err
		}
		reserve := int64(0)
		if pending {
			reserve = p.Spec.SizeBytes
		}
		tasks = append(tasks, cleanup{"photo", p.Spec.AttemptID, p.ObjectVersion, reserve})
	}
	for _, v := range videos {
		if v.Spec == nil {
			continue
		}
		amount, keep := v.Spec.MaxBytes, ""
		if v.Result != nil {
			amount, keep = v.Result.Object.SizeBytes, v.Result.Object.VersionID
		}
		if err := add(amount, v.Result == nil); err != nil {
			return DerivedFinalization{}, err
		}
		reserve := int64(0)
		if v.Result == nil {
			reserve = amount
		}
		tasks = append(tasks, cleanup{"video", v.ID, keep, reserve})
	}
	f.ReleasedBytes = f.BudgetBytes - f.RetainedBytes - f.HeldBytes
	if account.reserved < f.BudgetBytes-f.RetainedBytes {
		return DerivedFinalization{}, ErrQuota
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO album_derived_finalizations(owner_id,version_id,manifest_digest,budget_bytes,retained_bytes,held_bytes,released_bytes,closed_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, f.OwnerID, f.VersionID, f.ManifestDigest, f.BudgetBytes, f.RetainedBytes, f.HeldBytes, f.ReleasedBytes, now, now)
	if err != nil {
		return DerivedFinalization{}, err
	}
	for _, c := range tasks {
		var keep any
		if c.keep != "" {
			keep = c.keep
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO album_derived_cleanups(cleanup_id,owner_id,version_id,manifest_digest,kind,reference_id,reserve_bytes,keep_version,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, uuid.NewString(), u.OwnerID, u.ID, u.Digest, c.kind, c.ref, c.reserve, keep, now, now, now)
		if err != nil {
			return DerivedFinalization{}, err
		}
	}
	if f.ReleasedBytes > 0 {
		result, err := tx.ExecContext(ctx, `UPDATE album_storage_accounts SET reserved_bytes=reserved_bytes-? WHERE owner_id=? AND reserved_bytes>=?`, f.ReleasedBytes, u.OwnerID, f.ReleasedBytes)
		if err != nil {
			return DerivedFinalization{}, err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return DerivedFinalization{}, ErrQuota
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_uploads SET derived_budget_closed=TRUE WHERE owner_id=? AND upload_id=?`, u.OwnerID, u.ID); err != nil {
		return DerivedFinalization{}, err
	}
	return f, tx.Commit()
}

func terminalDerivedInventory(ctx context.Context, tx *sql.Tx, u Upload) ([]PhotoWriteRecord, []VideoJob, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+photoJobColumns+` FROM album_photo_jobs WHERE owner_id=? AND version_id=? FOR UPDATE`, u.OwnerID, u.ID)
	if err != nil {
		return nil, nil, err
	}
	var jobs []PhotoJob
	for rows.Next() {
		j, e := scanPhotoJob(rows)
		if e != nil {
			rows.Close()
			return nil, nil, e
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	for _, j := range jobs {
		if j.State != "completed" && j.State != "attention" {
			return nil, nil, ErrDerivedPending
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND version_id=? FOR UPDATE`, u.OwnerID, u.ID)
	if err != nil {
		return nil, nil, err
	}
	var photos []PhotoWriteRecord
	for rows.Next() {
		p, e := scanPhotoWrite(rows)
		if e != nil {
			rows.Close()
			return nil, nil, e
		}
		if p.ManifestDigest != u.Digest || !p.RetirementStartedAt.IsZero() || !p.RetiredAt.IsZero() {
			rows.Close()
			return nil, nil, ErrConflict
		}
		photos = append(photos, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND version_id=? FOR UPDATE`, u.OwnerID, u.ID)
	if err != nil {
		return nil, nil, err
	}
	var videos []VideoJob
	for rows.Next() {
		v, e := scanVideoJob(rows)
		if e != nil {
			rows.Close()
			return nil, nil, e
		}
		if v.State != "completed" && v.State != "attention" {
			rows.Close()
			return nil, nil, ErrDerivedPending
		}
		if v.ManifestDigest != u.Digest || !v.RetirementStartedAt.IsZero() || !v.RetiredAt.IsZero() {
			rows.Close()
			return nil, nil, ErrConflict
		}
		videos = append(videos, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	switch u.Manifest.Kind {
	case Photo, LivePhoto:
		expected := photoJobPresets(u.Manifest)
		expectedVideos := 0
		if livePhotoPreviewEligible(u.Manifest) {
			expectedVideos = 1
		}
		if len(jobs) != len(expected) || len(videos) != expectedVideos || (expectedVideos == 1 && videos[0].Preset != VideoDaily1080) {
			return nil, nil, ErrDerivedPending
		}
		for _, preset := range expected {
			matched := false
			for _, j := range jobs {
				if j.Preset == preset {
					matched = true
					if j.State == "completed" {
						ready := false
						for _, p := range photos {
							if p.Spec.Preset == preset && p.ObjectVersion != "" {
								ready = true
							}
						}
						if !ready {
							return nil, nil, ErrConflict
						}
					}
				}
			}
			if !matched {
				return nil, nil, ErrDerivedPending
			}
		}
	case Video:
		if len(videos) != 1 || len(jobs) != 0 || len(photos) != 0 || videos[0].Preset != VideoDaily1080 {
			return nil, nil, ErrDerivedPending
		}
	default:
		return nil, nil, ErrConflict
	}
	return photos, videos, nil
}
