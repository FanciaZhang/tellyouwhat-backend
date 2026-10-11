package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const livePairColumns = `pair_upload_id,owner_id,request_id,asset_id,version_id,manifest_digest,preset,descriptor_json,source_photo_json,source_video_json,objects_json,size_bytes,state,photo_consumed,movie_consumed,expires_at,lease_token,lease_until,attempts`

func scanLivePair(row rowScanner) (LivePairUpload, error) {
	var u LivePairUpload
	var descriptor, photo, video, objects []byte
	var size int64
	var token sql.NullString
	var lease sql.NullTime
	err := row.Scan(&u.ID, &u.OwnerID, &u.RequestID, &u.AssetID, &u.VersionID, &u.ManifestDigest, &u.Preset, &descriptor, &photo, &video, &objects, &size,
		&u.State, &u.PhotoConsumed, &u.MovieConsumed, &u.ExpiresAt, &token, &lease, &u.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	u.LeaseToken, u.LeaseUntil = token.String, lease.Time
	if len(descriptor) > 16384 || len(photo) > 8192 || len(video) > 8192 || len(objects) > 16384 ||
		json.Unmarshal(descriptor, &u.Descriptor) != nil || json.Unmarshal(photo, &u.SourcePhoto) != nil || json.Unmarshal(video, &u.SourceVideo) != nil || json.Unmarshal(objects, &u.Objects) != nil ||
		!validOwner(u.ID) || !validOwner(u.OwnerID) || !validOwner(u.RequestID) || !validOwner(u.VersionID) || !resourceIDPattern.MatchString(u.AssetID) || !validSHA256(u.ManifestDigest) ||
		u.Preset != DailyLivePair || u.Descriptor.validate() != nil || size != u.Descriptor.Photo.SizeBytes+u.Descriptor.Movie.SizeBytes || u.ExpiresAt.IsZero() || u.Attempts < 0 || u.Attempts > 8 ||
		(u.State != "uploading" && u.State != "queued" && u.State != "verifying" && u.State != "verified" && u.State != "attention" && u.State != "retiring" && u.State != "retired") ||
		(u.State == "verifying" && (!validOwner(u.LeaseToken) || u.LeaseUntil.IsZero())) || len(u.Objects) > 2 {
		return LivePairUpload{}, ErrConflict
	}
	seen := map[string]bool{}
	for _, object := range u.Objects {
		if u.validateReceipt(object) != nil || seen[object.ResourceID] || (object.ResourceID == "photo" && !u.PhotoConsumed) || (object.ResourceID == "motion" && !u.MovieConsumed) {
			return LivePairUpload{}, ErrConflict
		}
		seen[object.ResourceID] = true
	}
	if u.State == "verified" && len(u.Objects) != 2 {
		return LivePairUpload{}, ErrConflict
	}
	return u, nil
}
func (r *MySQLUploads) CheckLivePairSchema(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `SELECT `+livePairColumns+` FROM album_live_pairs LIMIT 0`)
	if err != nil {
		return err
	}
	return rows.Close()
}
func (r *MySQLUploads) LivePair(ctx context.Context, owner, version string) (LivePairUpload, error) {
	if !validOwner(owner) || !validOwner(version) {
		return LivePairUpload{}, ErrInvalidManifest
	}
	return scanLivePair(r.db.QueryRowContext(ctx, `SELECT `+livePairColumns+` FROM album_live_pairs WHERE owner_id=? AND version_id=? AND active_preset=?`, owner, version, DailyLivePair))
}
func (r *MySQLUploads) LivePairByID(ctx context.Context, owner, id string) (LivePairUpload, error) {
	if !validOwner(owner) || !validOwner(id) {
		return LivePairUpload{}, ErrInvalidManifest
	}
	return scanLivePair(r.db.QueryRowContext(ctx, `SELECT `+livePairColumns+` FROM album_live_pairs WHERE owner_id=? AND pair_upload_id=?`, owner, id))
}
func (r *MySQLUploads) ReserveLivePair(ctx context.Context, u LivePairUpload, now time.Time) (LivePairUpload, error) {
	if !validOwner(u.ID) || !validOwner(u.OwnerID) || !validOwner(u.VersionID) || !validOwner(u.RequestID) || u.Preset != DailyLivePair || u.State != "uploading" ||
		u.Descriptor.validate() != nil || !u.ExpiresAt.After(now) || u.ExpiresAt.After(now.Add(24*time.Hour)) || len(u.Objects) != 0 || u.PhotoConsumed || u.MovieConsumed {
		return LivePairUpload{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return LivePairUpload{}, err
	}
	defer tx.Rollback()
	account, err := lockAccount(ctx, tx, u.OwnerID)
	if err != nil {
		return LivePairUpload{}, err
	}
	old, err := scanLivePair(tx.QueryRowContext(ctx, `SELECT `+livePairColumns+` FROM album_live_pairs WHERE owner_id=? AND request_id=? FOR UPDATE`, u.OwnerID, u.RequestID))
	if err == nil {
		if old.VersionID != u.VersionID || old.Descriptor != u.Descriptor || old.ManifestDigest != u.ManifestDigest || old.State == "retired" || old.State == "retiring" {
			return LivePairUpload{}, ErrConflict
		}
		return old, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return LivePairUpload{}, err
	}
	old, err = scanLivePair(tx.QueryRowContext(ctx, `SELECT `+livePairColumns+` FROM album_live_pairs WHERE owner_id=? AND version_id=? AND active_preset=? FOR UPDATE`, u.OwnerID, u.VersionID, DailyLivePair))
	if err == nil {
		if old.ManifestDigest != u.ManifestDigest || (old.State != "verified" && (old.Descriptor != u.Descriptor || old.RequestID != u.RequestID)) {
			return LivePairUpload{}, ErrConflict
		}
		return old, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return LivePairUpload{}, err
	}
	// Hold component rows through reservation. A retirement fence or changed
	// provider receipt must not race a client-supplied declaration into storage.
	v, err := scanAssetVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND version_id=? FOR UPDATE`, u.OwnerID, u.VersionID))
	if err != nil {
		return LivePairUpload{}, err
	}
	if err = validateVersionOwner(v, u.OwnerID); err != nil {
		return LivePairUpload{}, err
	}
	p, err := scanPhotoWrite(tx.QueryRowContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND version_id=? AND preset=? FOR UPDATE`, u.OwnerID, u.VersionID, PhotoDailyJPEG))
	if err != nil {
		return LivePairUpload{}, err
	}
	j, err := scanVideoJob(tx.QueryRowContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND version_id=? AND preset=? FOR UPDATE`, u.OwnerID, u.VersionID, VideoDaily1080))
	if err != nil {
		return LivePairUpload{}, err
	}
	if err := livePairSources(u, v, p, j); err != nil {
		return LivePairUpload{}, err
	}
	size := u.Descriptor.Photo.SizeBytes + u.Descriptor.Movie.SizeBytes
	if account.limit < 0 || account.used < 0 || account.reserved < 0 || account.used > account.limit || account.reserved > account.limit-account.used || size > account.limit-account.used-account.reserved {
		return LivePairUpload{}, ErrQuota
	}
	descriptor, _ := json.Marshal(u.Descriptor)
	photo, _ := json.Marshal(u.SourcePhoto)
	video, _ := json.Marshal(u.SourceVideo)
	_, err = tx.ExecContext(ctx, `INSERT INTO album_live_pairs(pair_upload_id,owner_id,request_id,asset_id,version_id,manifest_digest,preset,active_preset,descriptor_json,source_photo_json,source_video_json,objects_json,size_bytes,state,expires_at,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,'[]',?,?,?,?,?,?)`,
		u.ID, u.OwnerID, u.RequestID, u.AssetID, u.VersionID, u.ManifestDigest, u.Preset, u.Preset, descriptor, photo, video, size, u.State, u.ExpiresAt, now, now, now)
	if err != nil {
		return LivePairUpload{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE album_storage_accounts SET reserved_bytes=reserved_bytes+? WHERE owner_id=?`, size, u.OwnerID)
	if err != nil {
		return LivePairUpload{}, err
	}
	return u, tx.Commit()
}

func (r *MySQLUploads) mutateLivePair(ctx context.Context, owner, id string, fn func(*sql.Tx, LivePairUpload) (LivePairUpload, error)) (LivePairUpload, error) {
	if !validOwner(owner) || !validOwner(id) {
		return LivePairUpload{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return LivePairUpload{}, err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, owner); err != nil {
		return LivePairUpload{}, err
	}
	u, err := scanLivePair(tx.QueryRowContext(ctx, `SELECT `+livePairColumns+` FROM album_live_pairs WHERE owner_id=? AND pair_upload_id=? FOR UPDATE`, owner, id))
	if err != nil {
		return LivePairUpload{}, err
	}
	u, err = fn(tx, u)
	if err != nil {
		return LivePairUpload{}, err
	}
	return u, tx.Commit()
}
func (r *MySQLUploads) SubmitLivePair(ctx context.Context, owner, id string, now time.Time) (LivePairUpload, error) {
	return r.mutateLivePair(ctx, owner, id, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		if u.State == "verified" || u.State == "queued" || u.State == "verifying" {
			return u, nil
		}
		if u.State != "uploading" {
			return u, ErrConflict
		}
		if !u.ExpiresAt.After(now) {
			return u, ErrExpired
		}
		_, err := tx.ExecContext(ctx, `UPDATE album_live_pairs SET state='queued',available_at=?,updated_at=? WHERE pair_upload_id=?`, now, now, id)
		u.State = "queued"
		return u, err
	})
}
func (r *MySQLUploads) PendingLivePairs(ctx context.Context, now time.Time, limit int) ([]LivePairUpload, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+livePairColumns+` FROM album_live_pairs WHERE (state='queued' AND available_at<=?) OR (state='verifying' AND lease_until<=?) ORDER BY available_at,pair_upload_id LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []LivePairUpload
	for rows.Next() {
		u, err := scanLivePair(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, u)
	}
	return jobs, rows.Err()
}
func (r *MySQLUploads) ClaimLivePair(ctx context.Context, owner, id, token string, now time.Time, lease time.Duration) (LivePairUpload, error) {
	if !validOwner(token) || lease < time.Second || lease > time.Hour {
		return LivePairUpload{}, ErrInvalidManifest
	}
	u, err := r.mutateLivePair(ctx, owner, id, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		if (u.State != "queued" && u.State != "verifying") || (u.State == "verifying" && u.LeaseUntil.After(now)) {
			return u, ErrLease
		}
		var available time.Time
		if err := tx.QueryRowContext(ctx, `SELECT available_at FROM album_live_pairs WHERE pair_upload_id=?`, id).Scan(&available); err != nil {
			return u, err
		}
		if u.State == "queued" && available.After(now) {
			return u, ErrLease
		}
		if u.Attempts >= 8 || !u.ExpiresAt.Add(24*time.Hour).After(now) {
			_, err := tx.ExecContext(ctx, `UPDATE album_live_pairs SET state='attention',lease_token=NULL,lease_until=NULL,updated_at=? WHERE pair_upload_id=?`, now, id)
			u.State = "attention"
			return u, err
		}
		u.State, u.LeaseToken, u.LeaseUntil, u.Attempts = "verifying", token, now.Add(lease), u.Attempts+1
		_, err := tx.ExecContext(ctx, `UPDATE album_live_pairs SET state='verifying',lease_token=?,lease_until=?,attempts=?,updated_at=? WHERE pair_upload_id=?`, token, u.LeaseUntil, u.Attempts, now, id)
		return u, err
	})
	if err == nil && u.State == "attention" {
		return u, ErrLease
	}
	return u, err
}
func (r *MySQLUploads) BeginLivePairSeal(ctx context.Context, expected LivePairUpload, part string, now time.Time) (LivePairUpload, bool, error) {
	if _, err := expected.resource(part); err != nil {
		return LivePairUpload{}, false, err
	}
	fresh := false
	u, err := r.mutateLivePair(ctx, expected.OwnerID, expected.ID, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		if !pairLease(u, expected, now) {
			return u, ErrLease
		}
		column := "photo_consumed"
		consumed := u.PhotoConsumed
		if part == "motion" {
			column, consumed = "movie_consumed", u.MovieConsumed
		}
		if consumed {
			return u, nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE album_live_pairs SET `+column+`=TRUE,updated_at=? WHERE pair_upload_id=?`, now, u.ID)
		if part == "photo" {
			u.PhotoConsumed = true
		} else {
			u.MovieConsumed = true
		}
		fresh = true
		return u, err
	})
	if err != nil {
		return LivePairUpload{}, false, err
	}
	return u, fresh, nil
}
func (r *MySQLUploads) CompleteLivePairPart(ctx context.Context, expected LivePairUpload, object SealedObject, now time.Time) (LivePairUpload, error) {
	if expected.validateReceipt(object) != nil {
		return LivePairUpload{}, ErrInvalidManifest
	}
	return r.mutateLivePair(ctx, expected.OwnerID, expected.ID, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		if !pairLease(u, expected, now) || (object.ResourceID == "photo" && !u.PhotoConsumed) || (object.ResourceID == "motion" && !u.MovieConsumed) {
			return u, ErrLease
		}
		for _, old := range u.Objects {
			if old.ResourceID == object.ResourceID {
				if old != object {
					return u, ErrConflict
				}
				return u, nil
			}
		}
		u.Objects = append(u.Objects, object)
		encoded, _ := json.Marshal(u.Objects)
		_, err := tx.ExecContext(ctx, `UPDATE album_live_pairs SET objects_json=?,updated_at=? WHERE pair_upload_id=?`, encoded, now, u.ID)
		return u, err
	})
}
func (r *MySQLUploads) CompleteLivePair(ctx context.Context, expected LivePairUpload, now time.Time) (LivePairUpload, error) {
	return r.mutateLivePair(ctx, expected.OwnerID, expected.ID, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		if u.Descriptor != expected.Descriptor || u.ManifestDigest != expected.ManifestDigest || u.VersionID != expected.VersionID {
			return u, ErrConflict
		}
		if u.State == "verified" {
			return u, nil
		}
		if !pairLease(u, expected, now) || len(u.Objects) != 2 {
			return u, ErrLease
		}
		size := u.Descriptor.Photo.SizeBytes + u.Descriptor.Movie.SizeBytes
		result, err := tx.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=used_bytes+?,reserved_bytes=reserved_bytes-? WHERE owner_id=? AND reserved_bytes>=?`, size, size, u.OwnerID, size)
		if err != nil {
			return u, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return u, err
		}
		if count != 1 {
			return u, ErrQuota
		}
		_, err = tx.ExecContext(ctx, `UPDATE album_live_pairs SET state='verified',lease_token=NULL,lease_until=NULL,updated_at=? WHERE pair_upload_id=?`, now, u.ID)
		u.State, u.LeaseToken, u.LeaseUntil = "verified", "", time.Time{}
		return u, err
	})
}
func (r *MySQLUploads) DeferLivePair(ctx context.Context, expected LivePairUpload, now time.Time) error {
	_, err := r.mutateLivePair(ctx, expected.OwnerID, expected.ID, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		if !pairLease(u, expected, now) {
			return u, ErrLease
		}
		state := "queued"
		if u.Attempts >= 8 || !u.ExpiresAt.Add(24*time.Hour).After(now) {
			state = "attention"
		}
		_, err := tx.ExecContext(ctx, `UPDATE album_live_pairs SET state=?,available_at=?,lease_token=NULL,lease_until=NULL,updated_at=? WHERE pair_upload_id=?`, state, now.Add(time.Minute), now, u.ID)
		return u, err
	})
	return err
}
