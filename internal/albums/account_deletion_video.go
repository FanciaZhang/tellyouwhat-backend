package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

const videoDeletionLease = 2 * time.Minute

type videoDeletionJob struct {
	account, request, id, lease string
	attempts                    int
	video                       VideoJob
}
type videoDeletionBatch struct{ Acknowledged, Deferred, Contended int }

// Internal read-only provider phase. No new transcode, cancellation or media
// deletion is possible through the observer. Terminal receipts do not prove erasure.
func (r *MySQLUploads) runVideoDeletionBatch(ctx context.Context, observe func(context.Context, VideoJob) (VideoCleanupObservation, error), limit int, now func() time.Time) (videoDeletionBatch, error) {
	var result videoDeletionBatch
	if observe == nil || now == nil || limit < 1 || limit > 100 {
		return result, ErrInvalidManifest
	}
	at := now().UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return result, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT account_id,request_id,job_id FROM album_deletion_video_jobs WHERE acknowledged_at IS NULL AND available_at<=? AND (lease_until IS NULL OR lease_until<=?) ORDER BY available_at,job_id LIMIT ?`, at, at, limit)
	if err != nil {
		return result, err
	}
	var jobs []videoDeletionJob
	for rows.Next() {
		var j videoDeletionJob
		if err = rows.Scan(&j.account, &j.request, &j.id); err != nil {
			rows.Close()
			return result, err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, j := range jobs {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		j, err = r.claimVideoDeletion(ctx, j, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		observation, callErr := observe(call, j.video)
		expired := call.Err() != nil
		cancel()
		if err = ctx.Err(); err != nil {
			return result, err
		}
		// An expired request or malformed/foreign observation is never a receipt.
		acknowledged := callErr == nil && !expired && observation.Terminal && ciJobPattern.MatchString(observation.ProviderJobID) &&
			(j.video.ProviderJobID == "" || j.video.ProviderJobID == observation.ProviderJobID)
		if !acknowledged {
			observation = VideoCleanupObservation{}
		}
		err = r.finishVideoDeletion(ctx, j, observation, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		if acknowledged {
			result.Acknowledged++
		} else {
			result.Deferred++
		}
	}
	return result, nil
}

func (r *MySQLUploads) lockDeletionVideo(ctx context.Context, tx *sql.Tx, ref videoDeletionJob) (VideoJob, error) {
	if !validOwner(ref.account) || !validOwner(ref.request) || !validOwner(ref.id) {
		return VideoJob{}, ErrConflict
	}
	// Keep the same account -> storage -> media ordering as deletion acceptance.
	if err := lockDeletingAccount(ctx, tx, ref.account, ref.request); err != nil {
		return VideoJob{}, err
	}
	if _, err := lockAccount(ctx, tx, ref.account); err != nil {
		return VideoJob{}, err
	}
	var requested, fenced sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT d.requested_at,s.deletion_requested_at FROM album_account_deletions d JOIN album_storage_accounts s ON s.owner_id=d.account_id WHERE d.account_id=? AND d.request_id=?`, ref.account, ref.request).Scan(&requested, &fenced); err != nil {
		return VideoJob{}, err
	}
	if !requested.Valid || !fenced.Valid || !requested.Time.Equal(fenced.Time) {
		return VideoJob{}, ErrConflict
	}
	j, err := scanVideoJob(tx.QueryRowContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND job_id=? FOR UPDATE`, ref.account, ref.id))
	if err != nil {
		return j, err
	}
	if j.Spec == nil {
		return j, ErrConflict
	}
	v, err := scanAssetVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND version_id=?`, j.OwnerID, j.VersionID))
	if err != nil {
		return j, err
	}
	source, err := videoSource(v)
	if err != nil || v.ManifestDigest != j.ManifestDigest || source != j.Spec.Source || !sameLiveTiming(v.Manifest.LivePhotoTiming, j.Spec.LivePhotoTiming) {
		return j, ErrConflict
	}
	return j, nil
}

func (r *MySQLUploads) claimVideoDeletion(ctx context.Context, ref videoDeletionJob, at time.Time) (videoDeletionJob, error) {
	if at.IsZero() {
		return ref, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ref, err
	}
	defer tx.Rollback()
	ref.video, err = r.lockDeletionVideo(ctx, tx, ref)
	if err != nil {
		return ref, err
	}
	ref.lease = uuid.NewString()
	result, err := tx.ExecContext(ctx, `UPDATE album_deletion_video_jobs SET lease_token=?,lease_until=?,attempts=LEAST(attempts+1,1000000) WHERE job_id=? AND account_id=? AND request_id=? AND acknowledged_at IS NULL AND available_at<=? AND (lease_until IS NULL OR lease_until<=?)`, ref.lease, at.Add(videoDeletionLease), ref.id, ref.account, ref.request, at, at)
	if err != nil {
		return ref, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return ref, ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT attempts FROM album_deletion_video_jobs WHERE job_id=?`, ref.id).Scan(&ref.attempts); err != nil {
		return ref, err
	}
	return ref, tx.Commit()
}

func (r *MySQLUploads) finishVideoDeletion(ctx context.Context, ref videoDeletionJob, observation VideoCleanupObservation, at time.Time) error {
	if !validOwner(ref.lease) || at.IsZero() || ref.attempts < 1 {
		return ErrInvalidManifest
	}
	if observation.Terminal && !ciJobPattern.MatchString(observation.ProviderJobID) || !observation.Terminal && (observation.ProviderJobID != "" || observation.Succeeded) {
		return ErrConflict
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := r.lockDeletionVideo(ctx, tx, ref)
	if err != nil {
		return err
	}
	if ref.video.Spec == nil || j.ID != ref.video.ID || j.OwnerID != ref.video.OwnerID || j.VersionID != ref.video.VersionID || j.ManifestDigest != ref.video.ManifestDigest || !j.SubmittedAt.Equal(ref.video.SubmittedAt) || !j.Spec.matches(*ref.video.Spec) ||
		observation.Terminal && j.ProviderJobID != "" && j.ProviderJobID != observation.ProviderJobID {
		return ErrConflict
	}
	var ack, provider, succeeded any
	if observation.Terminal {
		ack, provider, succeeded = at, observation.ProviderJobID, observation.Succeeded
	}
	result, err := tx.ExecContext(ctx, `UPDATE album_deletion_video_jobs SET acknowledged_at=?,provider_job_id=?,provider_succeeded=?,available_at=?,lease_token=NULL,lease_until=NULL WHERE job_id=? AND account_id=? AND request_id=? AND acknowledged_at IS NULL AND lease_token=? AND lease_until>?`, ack, provider, succeeded, at.Add(accountDeletionRetry(ref.attempts)), ref.id, ref.account, ref.request, ref.lease, at)
	if err != nil {
		return err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return ErrConflict
	}
	if observation.Terminal && j.ProviderJobID == "" {
		if _, err = tx.ExecContext(ctx, `UPDATE album_video_jobs SET provider_job_id=?,updated_at=? WHERE job_id=? AND owner_id=? AND provider_job_id IS NULL`, observation.ProviderJobID, at, j.ID, j.OwnerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
