package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

type PhotoJob struct {
	ID, OwnerID, VersionID string
	Preset                 PhotoPreset
	State                  string
	Attempts               int
	LeaseToken             string
	LeaseUntil             time.Time
}

const photoJobColumns = `job_id,owner_id,version_id,preset,state,attempts,lease_token,lease_until`

func scanPhotoJob(row rowScanner) (PhotoJob, error) {
	var j PhotoJob
	var token sql.NullString
	var until sql.NullTime
	err := row.Scan(&j.ID, &j.OwnerID, &j.VersionID, &j.Preset, &j.State, &j.Attempts, &token, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	j.LeaseToken = token.String
	j.LeaseUntil = until.Time
	if !validOwner(j.ID) || !validOwner(j.OwnerID) || !validOwner(j.VersionID) || j.Attempts < 0 || j.Attempts > 8 {
		return PhotoJob{}, ErrConflict
	}
	if rule, _, _, _ := photoPresetRule(j.Preset); rule == "" {
		return PhotoJob{}, ErrConflict
	}
	return j, nil
}

// Called within original publication, including idempotent Complete replay.
// These are still-image candidates; a Live Photo additionally needs its movie
// and pairing. Non-JPEG uses PNG to avoid silently flattening transparency.
func photoJobPresets(manifest Manifest) []PhotoPreset {
	if livePhotoPreviewEligible(manifest) {
		return []PhotoPreset{PhotoGridJPEG, PhotoDailyJPEG}
	}
	role := OriginalPhoto
	if manifest.Edited {
		role = CurrentPhoto
	}
	presets := []PhotoPreset{PhotoGridPNG, PhotoDailyPNG}
	for _, r := range manifest.Resources {
		if r.Role == role && r.Descriptor != nil && r.Descriptor.UniformTypeIdentifier == "public.jpeg" {
			presets = []PhotoPreset{PhotoGridJPEG, PhotoDailyJPEG}
		}
	}
	return presets
}
func enqueuePhotoJobs(ctx context.Context, tx *sql.Tx, u Upload, now time.Time) error {
	if u.Manifest.Kind != Photo && u.Manifest.Kind != LivePhoto {
		return nil
	}
	presets := photoJobPresets(u.Manifest)
	for _, preset := range presets {
		_, err := tx.ExecContext(ctx, `INSERT INTO album_photo_jobs(job_id,owner_id,version_id,preset,available_at,updated_at) VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE job_id=job_id`, uuid.NewString(), u.OwnerID, u.ID, preset, now, now)
		if err != nil {
			return err
		}
	}
	return nil
}
func (r *MySQLUploads) PendingPhotoJobs(ctx context.Context, now time.Time, limit int) ([]PhotoJob, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+photoJobColumns+` FROM album_photo_jobs j WHERE ((state='queued' AND available_at<=?) OR (state='running' AND lease_until<=?)) AND EXISTS(SELECT 1 FROM album_asset_versions v JOIN album_asset_lifecycle a ON a.owner_id=v.owner_id AND a.asset_id=v.asset_id WHERE v.owner_id=j.owner_id AND v.version_id=j.version_id AND a.state='active') ORDER BY available_at,CASE WHEN preset LIKE 'grid_%' THEN 0 ELSE 1 END,job_id LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]PhotoJob, 0, limit)
	for rows.Next() {
		j, err := scanPhotoJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, j)
	}
	return result, rows.Err()
}
func (r *MySQLUploads) ClaimPhotoJob(ctx context.Context, owner, id, token string, now time.Time) (PhotoJob, error) {
	if !validOwner(owner) || !validOwner(id) || !validOwner(token) {
		return PhotoJob{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return PhotoJob{}, err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, owner); err != nil {
		if errors.Is(err, ErrOwner) {
			return PhotoJob{}, ErrLease
		}
		return PhotoJob{}, err
	}
	j, err := scanPhotoJob(tx.QueryRowContext(ctx, `SELECT `+photoJobColumns+` FROM album_photo_jobs WHERE owner_id=? AND job_id=? AND ((state='queued' AND available_at<=?) OR (state='running' AND lease_until<=?)) FOR UPDATE`, owner, id, now, now))
	if errors.Is(err, ErrNotFound) {
		return PhotoJob{}, ErrLease
	}
	if err != nil {
		return PhotoJob{}, err
	}
	if err := requireVisibleVersion(ctx, tx, owner, j.VersionID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return PhotoJob{}, ErrLease
		}
		return PhotoJob{}, err
	}
	if j.Attempts >= 8 {
		if _, err := tx.ExecContext(ctx, `UPDATE album_photo_jobs SET state='attention',lease_token=NULL,lease_until=NULL,updated_at=? WHERE job_id=?`, now, id); err != nil {
			return PhotoJob{}, err
		}
		if err := tx.Commit(); err != nil {
			return PhotoJob{}, err
		}
		return PhotoJob{}, ErrLease
	}
	j.Attempts++
	j.State = "running"
	j.LeaseToken = token
	j.LeaseUntil = now.Add(10 * time.Minute)
	_, err = tx.ExecContext(ctx, `UPDATE album_photo_jobs SET state='running',attempts=?,lease_token=?,lease_until=?,updated_at=? WHERE job_id=?`, j.Attempts, token, j.LeaseUntil, now, id)
	if err != nil {
		return PhotoJob{}, err
	}
	return j, tx.Commit()
}
func (r *MySQLUploads) FinishPhotoJob(ctx context.Context, j PhotoJob, success, terminal bool, now time.Time) error {
	if !validOwner(j.OwnerID) || !validOwner(j.ID) || !validOwner(j.LeaseToken) || j.Attempts < 1 || j.Attempts > 8 {
		return ErrInvalidManifest
	}
	state := "queued"
	delay := 30 * time.Second * time.Duration(1<<uint(j.Attempts-1))
	if success {
		state = "completed"
	} else if terminal || j.Attempts >= 8 {
		state = "attention"
	}
	result, err := r.db.ExecContext(ctx, `UPDATE album_photo_jobs SET state=?,available_at=?,lease_token=NULL,lease_until=NULL,updated_at=? WHERE owner_id=? AND job_id=? AND state='running' AND lease_token=? AND lease_until>?`, state, now.Add(delay), now, j.OwnerID, j.ID, j.LeaseToken, now)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLease
	}
	return nil
}
func (r *MySQLUploads) CheckPhotoSchema(ctx context.Context) error {
	for _, query := range []string{`SELECT ` + photoJobColumns + ` FROM album_photo_jobs LIMIT 0`, `SELECT ` + photoWriteColumns + ` FROM album_photo_writes LIMIT 0`} {
		rows, err := r.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		rows.Close()
	}
	return nil
}
