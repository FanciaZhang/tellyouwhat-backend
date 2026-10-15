package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Call under the account and storage locks. Use current reads, including when
// the caller established an older repeatable-read snapshot before those locks.
// Covers unpublished media too; publication and quota settlement are not proof
// that every issued provider operation has stopped.
func requireAccountMediaQuiescence(ctx context.Context, tx *sql.Tx, owner, request string) error {
	for _, query := range []string{
		`SELECT 1 FROM album_uploads WHERE owner_id=? AND direct_upload_issued=TRUE LIMIT 1 FOR UPDATE`,
		`SELECT 1 FROM album_live_pairs WHERE owner_id=? AND direct_upload_issued=TRUE LIMIT 1 FOR UPDATE`,
		`SELECT 1 FROM album_uploads u JOIN album_original_seals s ON s.upload_id=u.upload_id WHERE u.owner_id=? AND (s.legacy=TRUE OR s.object_version IS NULL OR s.manifest_digest<>u.manifest_digest) LIMIT 1 FOR UPDATE`,
		`SELECT 1 FROM album_uploads u JOIN album_multipart_intents i ON i.upload_id=u.upload_id
		 LEFT JOIN album_multipart_completions c ON c.upload_id=i.upload_id AND c.resource_id=i.resource_id
		 WHERE u.owner_id=? AND (i.state<>'ready' OR i.provider_upload_id IS NULL OR NOT ((
		 (c.state='assembled' AND COALESCE(c.object_version,'') NOT IN ('','null')) OR
		 (i.deletion_aborted_at IS NOT NULL AND i.deletion_abort_id IS NOT NULL AND (c.state IS NULL OR c.state='retryable'))
		 ) IS TRUE)) LIMIT 1 FOR UPDATE`,
		`SELECT 1 FROM album_live_pairs p JOIN album_live_pair_multiparts m ON m.pair_upload_id=p.pair_upload_id
		 WHERE p.owner_id=? AND (m.state NOT IN ('assembled','aborted') OR (m.state='assembled' AND COALESCE(m.object_version,'') IN ('','null')))
		 LIMIT 1 FOR UPDATE`,
		`SELECT 1 FROM album_live_pairs WHERE owner_id=? AND ((photo_consumed=TRUE AND photo_copy_version IS NULL) OR (movie_consumed=TRUE AND movie_copy_version IS NULL)) LIMIT 1 FOR UPDATE`,
	} {
		var found int
		err := tx.QueryRowContext(ctx, query, owner).Scan(&found)
		if err == nil {
			return ErrDerivedPending
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	// A missing terminal-observation job must not turn an issued transcode into
	// an empty queue. Acknowledgements bind the accepted request and provider ID.
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM album_video_jobs v LEFT JOIN album_deletion_video_jobs d
	 ON d.job_id=v.job_id AND d.account_id=v.owner_id AND d.request_id=?
	 WHERE v.owner_id=? AND v.spec_json IS NOT NULL AND
	 (d.acknowledged_at IS NULL OR d.provider_job_id IS NULL OR v.provider_job_id IS NULL OR d.provider_job_id<>v.provider_job_id)
	 LIMIT 1 FOR UPDATE`, request, owner).Scan(&found)
	if err == nil {
		return ErrDerivedPending
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Retirement may have removed the observed PUT version already. Its durable
	// terminal plan must still contain a committed object, not just a timestamp.
	rows, err := tx.QueryContext(ctx, `SELECT p.object_version,p.deletion_object_version,p.retirement_started_at,c.plan_json
	 FROM album_photo_writes p LEFT JOIN album_derived_cleanups c
	 ON c.owner_id=p.owner_id AND c.reference_id=p.attempt_id AND c.kind='photo'
	 AND c.version_id=p.version_id AND c.manifest_digest=p.manifest_digest AND c.keep_version IS NULL AND c.reserve_bytes=p.size_bytes
	 WHERE p.owner_id=? FOR UPDATE`, owner)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var verified, observed sql.NullString
		var retiring sql.NullTime
		var data []byte
		if err = rows.Scan(&verified, &observed, &retiring, &data); err != nil {
			return err
		}
		if concreteCOSVersion(verified.String) || concreteCOSVersion(observed.String) {
			continue
		}
		var plan DerivedCleanupPlan
		if !retiring.Valid || len(data) > 262144 || json.Unmarshal(data, &plan) != nil || validateCleanupPlan(plan) != nil || !plan.Terminal {
			return ErrDerivedPending
		}
		committed := false
		for _, version := range plan.Versions {
			committed = committed || !version.DeleteMarker
		}
		if !committed {
			return ErrDerivedPending
		}
	}
	return rows.Err()
}

// This transaction makes no network calls. Account/storage locks serialize it
// with write admission and late receipts; no expiring worker lease is needed.
func (r *MySQLUploads) checkAccountMediaQuiescence(ctx context.Context, owner, request string, at time.Time) (ready, waiting bool, err error) {
	if !validOwner(owner) || !validOwner(request) || at.IsZero() {
		return false, false, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	if err = lockDeletingAccount(ctx, tx, owner, request); err != nil {
		return false, false, err
	}
	if _, err = lockAccount(ctx, tx, owner); err != nil {
		return false, false, err
	}
	var requested time.Time
	var fenced, checked, due sql.NullTime
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT d.requested_at,s.deletion_requested_at,d.media_quiesced_at,d.media_check_after,d.media_check_attempts
	 FROM album_account_deletions d JOIN album_storage_accounts s ON s.owner_id=d.account_id
	 WHERE d.account_id=? AND d.request_id=? FOR UPDATE`, owner, request).Scan(&requested, &fenced, &checked, &due, &attempts)
	if err != nil {
		return false, false, err
	}
	if !fenced.Valid || !requested.Equal(fenced.Time) || at.Before(requested) {
		return false, false, ErrConflict
	}
	if checked.Valid || due.Valid && due.Time.After(at) {
		return false, false, tx.Commit()
	}
	err = requireAccountMediaQuiescence(ctx, tx, owner, request)
	if err != nil && !errors.Is(err, ErrDerivedPending) {
		return false, false, err
	}
	ready, waiting = err == nil, err != nil
	var receipt any
	if ready {
		receipt = at
	}
	_, err = tx.ExecContext(ctx, `UPDATE album_account_deletions SET media_quiesced_at=?,media_check_after=?,media_check_attempts=LEAST(media_check_attempts+1,1000000) WHERE account_id=? AND request_id=?`, receipt, at.Add(accountDeletionRetry(attempts+1)), owner, request)
	if err != nil {
		return false, false, err
	}
	if err = tx.Commit(); err != nil {
		return false, false, err
	}
	return ready, waiting, nil
}

func (r *MySQLUploads) runAccountQuiescenceBatch(ctx context.Context, limit int, now func() time.Time) (ready, waiting int, err error) {
	if limit < 1 || limit > 100 || now == nil || now().IsZero() {
		return 0, 0, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT account_id,request_id FROM album_account_deletions
	 WHERE media_quiesced_at IS NULL AND (media_check_after IS NULL OR media_check_after<=?) ORDER BY media_check_after,account_id LIMIT ?`, now().UTC(), limit)
	if err != nil {
		return 0, 0, err
	}
	var refs [][2]string
	for rows.Next() {
		var ref [2]string
		if err = rows.Scan(&ref[0], &ref[1]); err != nil {
			rows.Close()
			return 0, 0, err
		}
		refs = append(refs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, err
	}
	for _, ref := range refs {
		r, w, e := r.checkAccountMediaQuiescence(ctx, ref[0], ref[1], now().UTC())
		if e != nil {
			return ready, waiting, e
		}
		if r {
			ready++
		}
		if w {
			waiting++
		}
	}
	return ready, waiting, nil
}
