package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

type VideoJob struct {
	ID, OwnerID, VersionID, ManifestDigest string
	Preset                                 VideoPreset
	State                                  string
	Failures                               int
	LeaseToken                             string
	LeaseUntil, Deadline, SubmittedAt      time.Time
	ProviderJobID                          string
	Spec                                   *VideoTranscodeSpec
	Result                                 *VideoResult
	RetirementStartedAt, RetiredAt         time.Time
}

const videoJobColumns = `job_id,owner_id,version_id,manifest_digest,preset,state,failures,lease_token,lease_until,deadline_at,submitted_at,provider_job_id,spec_json,result_json,reserve_size,retirement_started_at,retired_at`

func scanVideoJob(row rowScanner) (VideoJob, error) {
	var j VideoJob
	var token, provider sql.NullString
	var lease, deadline, submitted sql.NullTime
	var spec, result []byte
	var reserve int64
	var retiring, retired sql.NullTime
	err := row.Scan(&j.ID, &j.OwnerID, &j.VersionID, &j.ManifestDigest, &j.Preset, &j.State, &j.Failures, &token, &lease, &deadline, &submitted, &provider, &spec, &result, &reserve, &retiring, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	j.LeaseToken, j.LeaseUntil, j.Deadline, j.SubmittedAt, j.ProviderJobID = token.String, lease.Time, deadline.Time, submitted.Time, provider.String
	j.RetirementStartedAt, j.RetiredAt = retiring.Time, retired.Time
	if _, err := j.outputKey(); err != nil || !validSHA256(j.ManifestDigest) || j.Failures < 0 || j.Failures > 8 || len(spec) > 16384 || len(result) > 8192 ||
		(j.State != "queued" && j.State != "running" && j.State != "completed" && j.State != "attention") ||
		(provider.Valid && !ciJobPattern.MatchString(provider.String)) || (j.State == "running" && (!validOwner(j.LeaseToken) || j.LeaseUntil.IsZero() || j.Deadline.IsZero())) {
		return VideoJob{}, ErrConflict
	}
	if len(spec) > 0 {
		j.Spec = &VideoTranscodeSpec{}
		if json.Unmarshal(spec, j.Spec) != nil || j.validateSpec(*j.Spec) != nil || j.SubmittedAt.IsZero() || reserve != j.Spec.MaxBytes {
			return VideoJob{}, ErrConflict
		}
	} else if !j.SubmittedAt.IsZero() || reserve != 0 || provider.Valid {
		return VideoJob{}, ErrConflict
	}
	if len(result) > 0 {
		j.Result = &VideoResult{}
		if json.Unmarshal(result, j.Result) != nil || j.validateResult(*j.Result) != nil || j.State != "completed" || !provider.Valid {
			return VideoJob{}, ErrConflict
		}
	} else if j.State == "completed" {
		return VideoJob{}, ErrConflict
	}
	if (retired.Valid && !retiring.Valid) || (retiring.Valid && (j.Spec == nil || j.Result != nil || j.State != "attention")) {
		return VideoJob{}, ErrConflict
	}
	return j, nil
}

func enqueueVideoJobs(ctx context.Context, tx *sql.Tx, u Upload, now time.Time) error {
	if u.Manifest.Kind != Video && !livePhotoPreviewEligible(u.Manifest) {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO album_video_jobs(job_id,owner_id,version_id,manifest_digest,preset,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE job_id=job_id`, uuid.NewString(), u.OwnerID, u.ID, u.Digest, VideoDaily1080, now, now, now)
	return err
}

func enqueuePreviewJobs(ctx context.Context, tx *sql.Tx, u Upload, now time.Time) error {
	if err := enqueuePhotoJobs(ctx, tx, u, now); err != nil {
		return err
	}
	return enqueueVideoJobs(ctx, tx, u, now)
}

func (r *MySQLUploads) VideoJob(ctx context.Context, owner, version string, preset VideoPreset) (VideoJob, error) {
	if !validOwner(owner) || !validOwner(version) || preset != VideoDaily1080 {
		return VideoJob{}, ErrInvalidManifest
	}
	return scanVideoJob(r.db.QueryRowContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND version_id=? AND preset=?`, owner, version, preset))
}

func (r *MySQLUploads) PendingVideoJobs(ctx context.Context, now time.Time, limit int) ([]VideoJob, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs j WHERE ((state='queued' AND available_at<=?) OR (state='running' AND lease_until<=?)) AND EXISTS(SELECT 1 FROM album_asset_versions v JOIN album_asset_lifecycle a ON a.owner_id=v.owner_id AND a.asset_id=v.asset_id WHERE v.owner_id=j.owner_id AND v.version_id=j.version_id AND a.state='active') ORDER BY available_at,job_id LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []VideoJob
	for rows.Next() {
		j, err := scanVideoJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (r *MySQLUploads) ClaimVideoJob(ctx context.Context, owner, id, token string, now time.Time) (VideoJob, error) {
	if !validOwner(owner) || !validOwner(id) || !validOwner(token) {
		return VideoJob{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return VideoJob{}, err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, owner); err != nil {
		if errors.Is(err, ErrOwner) {
			return VideoJob{}, ErrLease
		}
		return VideoJob{}, err
	}
	j, err := scanVideoJob(tx.QueryRowContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND job_id=? AND ((state='queued' AND available_at<=?) OR (state='running' AND lease_until<=?)) FOR UPDATE`, owner, id, now, now))
	if errors.Is(err, ErrNotFound) {
		return VideoJob{}, ErrLease
	}
	if err != nil {
		return VideoJob{}, err
	}
	if err := requireVisibleVersion(ctx, tx, owner, j.VersionID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return VideoJob{}, ErrLease
		}
		return VideoJob{}, err
	}
	if j.Failures >= 8 || (!j.Deadline.IsZero() && !j.Deadline.After(now)) {
		_, err = tx.ExecContext(ctx, `UPDATE album_video_jobs SET state='attention',lease_token=NULL,lease_until=NULL,updated_at=? WHERE job_id=?`, now, id)
		if err != nil {
			return VideoJob{}, err
		}
		if err := tx.Commit(); err != nil {
			return VideoJob{}, err
		}
		return VideoJob{}, ErrLease
	}
	if j.Deadline.IsZero() {
		j.Deadline = now.Add(24 * time.Hour)
	}
	j.State, j.LeaseToken, j.LeaseUntil = "running", token, now.Add(10*time.Minute)
	_, err = tx.ExecContext(ctx, `UPDATE album_video_jobs SET state='running',deadline_at=?,lease_token=?,lease_until=?,updated_at=? WHERE job_id=?`, j.Deadline, token, j.LeaseUntil, now, id)
	if err != nil {
		return VideoJob{}, err
	}
	return j, tx.Commit()
}

func videoLease(current, expected VideoJob, now time.Time) bool {
	return current.ID == expected.ID && current.OwnerID == expected.OwnerID && current.VersionID == expected.VersionID && current.ManifestDigest == expected.ManifestDigest &&
		current.Preset == expected.Preset && current.State == "running" && current.LeaseToken == expected.LeaseToken && validOwner(current.LeaseToken) &&
		current.LeaseUntil.After(now) && current.Deadline.After(now)
}

// Only a successful commit returning fresh=true grants the paid POST. An
// ambiguous commit must be reread; it never grants submission permission.
func (r *MySQLUploads) BeginVideoSubmission(ctx context.Context, expected VideoJob, spec VideoTranscodeSpec, now time.Time) (VideoJob, bool, error) {
	if expected.validateSpec(spec) != nil {
		return VideoJob{}, false, ErrInvalidManifest
	}
	var j VideoJob
	fresh := false
	_, err := r.mutate(ctx, expected.OwnerID, expected.VersionID, func(tx *sql.Tx, u Upload) (Upload, error) {
		if u.State != OriginalsVerified || u.Digest != expected.ManifestDigest {
			return u, ErrIncompleteBackup
		}
		var err error
		j, err = scanVideoJob(tx.QueryRowContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND job_id=? FOR UPDATE`, expected.OwnerID, expected.ID))
		if err != nil {
			return u, err
		}
		if !videoLease(j, expected, now) {
			return u, ErrLease
		}
		if j.Spec != nil {
			if !j.Spec.matches(spec) {
				return u, ErrConflict
			}
			return u, nil
		}
		if err := requireVisibleVersion(ctx, tx, u.OwnerID, u.ID); err != nil {
			return u, err
		}
		if err := derivedBudgetOpen(ctx, tx, u.OwnerID, u.ID); err != nil {
			return u, err
		}
		// Bind to the actual verified original, not a caller-supplied object path.
		v, err := scanAssetVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND version_id=?`, expected.OwnerID, expected.VersionID))
		if err != nil {
			return u, err
		}
		source, err := videoSource(v)
		if err != nil || source != spec.Source || !sameLiveTiming(spec.LivePhotoTiming, v.Manifest.LivePhotoTiming) {
			return u, ErrIncompleteBackup
		}
		var allocated int64
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COALESCE(SUM(size_bytes),0) FROM album_photo_writes WHERE owner_id=? AND version_id=?)+(SELECT COALESCE(SUM(reserve_size),0) FROM album_video_jobs WHERE owner_id=? AND version_id=?)`, j.OwnerID, j.VersionID, j.OwnerID, j.VersionID).Scan(&allocated); err != nil {
			return u, err
		}
		if allocated < 0 || allocated > u.DerivedBudget || spec.MaxBytes > u.DerivedBudget-allocated {
			return u, ErrQuota
		}
		data, err := json.Marshal(spec)
		if err != nil {
			return u, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE album_video_jobs SET submitted_at=?,spec_json=?,reserve_size=?,updated_at=? WHERE job_id=?`, now, data, spec.MaxBytes, now, j.ID)
		if err != nil {
			return u, err
		}
		j.Spec, j.SubmittedAt, fresh = &spec, now, true
		return u, nil
	})
	if err != nil {
		return VideoJob{}, false, err
	}
	return j, fresh, nil
}

func (r *MySQLUploads) AttachVideoSubmission(ctx context.Context, expected VideoJob, id string, now time.Time) (VideoJob, error) {
	if !ciJobPattern.MatchString(id) || expected.Spec == nil || expected.SubmittedAt.IsZero() || !validOwner(expected.LeaseToken) {
		return VideoJob{}, ErrInvalidManifest
	}
	result, err := r.db.ExecContext(ctx, `UPDATE album_video_jobs SET provider_job_id=?,updated_at=? WHERE owner_id=? AND job_id=? AND state='running' AND lease_token=? AND lease_until>? AND deadline_at>? AND submitted_at IS NOT NULL AND (provider_job_id IS NULL OR provider_job_id=?)`, id, now, expected.OwnerID, expected.ID, expected.LeaseToken, now, now, id)
	if err != nil {
		return VideoJob{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return VideoJob{}, err
	}
	if count != 1 {
		// MySQL may report zero changed rows for an idempotent attachment in the
		// same microsecond. Recheck both ownership and the still-current lease.
		current, readErr := r.VideoJob(ctx, expected.OwnerID, expected.VersionID, expected.Preset)
		if readErr == nil && current.ProviderJobID == id && videoLease(current, expected, now) {
			return current, nil
		}
		return VideoJob{}, ErrLease
	}
	expected.ProviderJobID = id
	return expected, nil
}

func (r *MySQLUploads) CompleteVideoJob(ctx context.Context, expected VideoJob, result VideoResult, now time.Time) (VideoJob, error) {
	if expected.validateResult(result) != nil || !ciJobPattern.MatchString(expected.ProviderJobID) {
		return VideoJob{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return VideoJob{}, err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, expected.OwnerID); err != nil {
		return VideoJob{}, err
	}
	j, err := scanVideoJob(tx.QueryRowContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND job_id=? FOR UPDATE`, expected.OwnerID, expected.ID))
	if err != nil {
		return VideoJob{}, err
	}
	if j.Spec == nil || !j.Spec.matches(*expected.Spec) || j.ProviderJobID != expected.ProviderJobID || j.ManifestDigest != expected.ManifestDigest {
		return VideoJob{}, ErrConflict
	}
	if !j.RetirementStartedAt.IsZero() || !j.RetiredAt.IsZero() {
		return VideoJob{}, ErrDerivedRetired
	}
	if j.Result != nil {
		if *j.Result != result {
			return VideoJob{}, ErrConflict
		}
		return j, tx.Commit()
	}
	if !videoLease(j, expected, now) {
		return VideoJob{}, ErrLease
	}
	j, err = publishVideoResult(ctx, tx, j, result, now)
	if err != nil {
		return VideoJob{}, err
	}
	return j, tx.Commit()
}

// Both an active processing lease and recovery under a closed-budget cleanup
// lease use the same atomic publication and quota transfer.
func publishVideoResult(ctx context.Context, tx *sql.Tx, j VideoJob, result VideoResult, now time.Time) (VideoJob, error) {
	transfer, err := tx.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=used_bytes+?,reserved_bytes=reserved_bytes-? WHERE owner_id=? AND reserved_bytes>=?`, result.Object.SizeBytes, result.Object.SizeBytes, j.OwnerID, result.Object.SizeBytes)
	if err != nil {
		return VideoJob{}, err
	}
	count, err := transfer.RowsAffected()
	if err != nil || count != 1 {
		return VideoJob{}, ErrQuota
	}
	if err := settleClosedDerivedWrite(ctx, tx, j.OwnerID, j.VersionID, j.ManifestDigest, "video", j.ID,
		j.Spec.MaxBytes, result.Object.SizeBytes, result.Object.VersionID, now); err != nil {
		return VideoJob{}, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return VideoJob{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE album_video_jobs SET state='completed',result_json=?,lease_token=NULL,lease_until=NULL,updated_at=? WHERE job_id=?`, data, now, j.ID)
	if err != nil {
		return VideoJob{}, err
	}
	j.State, j.Result, j.LeaseToken, j.LeaseUntil = "completed", &result, "", time.Time{}
	return j, nil
}

// Successful asynchronous waiting does not spend the transient failure budget.
// Unknown POSTs are searched at 15-minute intervals; known jobs poll once/minute.
func (r *MySQLUploads) FinishVideoStep(ctx context.Context, j VideoJob, failed, terminal bool, now time.Time) error {
	if !validOwner(j.OwnerID) || !validOwner(j.ID) || !validOwner(j.LeaseToken) {
		return ErrInvalidManifest
	}
	state, failures, delay := "queued", j.Failures, time.Minute
	if !j.SubmittedAt.IsZero() && j.ProviderJobID == "" {
		delay = 15 * time.Minute
	}
	if failed {
		failures++
		delay = time.Minute * time.Duration(1<<uint(min(failures, 8)-1))
	}
	if terminal || failures >= 8 || !j.Deadline.After(now) {
		state = "attention"
	}
	result, err := r.db.ExecContext(ctx, `UPDATE album_video_jobs SET state=?,failures=?,available_at=?,lease_token=NULL,lease_until=NULL,updated_at=? WHERE owner_id=? AND job_id=? AND state='running' AND lease_token=? AND lease_until>?`, state, min(failures, 8), now.Add(delay), now, j.OwnerID, j.ID, j.LeaseToken, now)
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

func (r *MySQLUploads) CheckVideoSchema(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs LIMIT 0`)
	if err != nil {
		return err
	}
	return rows.Close()
}
