package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func sealResource(u Upload, id string) bool {
	for _, resource := range u.Manifest.Resources {
		if resource.ID == id {
			return true
		}
	}
	return false
}

// An advisory read lets retries use the archive even after staging is gone.
func (r *MySQLUploads) OriginalSeal(ctx context.Context, expected Upload, resource string) (string, bool, error) {
	if !validOwner(expected.OwnerID) || !validOwner(expected.ID) || !sealResource(expected, resource) {
		return "", false, ErrInvalidManifest
	}
	var version sql.NullString
	var digest string
	err := r.db.QueryRowContext(ctx, `SELECT s.manifest_digest,s.object_version FROM album_original_seals s JOIN album_uploads u ON u.upload_id=s.upload_id WHERE u.owner_id=? AND s.upload_id=? AND s.resource_id=?`, expected.OwnerID, expected.ID, resource).Scan(&digest, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if digest != expected.Digest || version.Valid && !concreteCOSVersion(version.String) {
		return "", false, ErrConflict
	}
	return version.String, true, nil
}

// Each resource consumes one COPY intent. A replacement lease may only observe
// that intent; a missing provider object is not permission to issue another COPY.
func (r *MySQLUploads) BeginOriginalSeal(ctx context.Context, expected Upload, resource string, now time.Time) (string, bool, error) {
	if !validOwner(expected.OwnerID) || !validOwner(expected.ID) || !sealResource(expected, resource) || now.IsZero() {
		return "", false, ErrInvalidManifest
	}
	var version string
	fresh := false
	_, err := r.mutate(ctx, expected.OwnerID, expected.ID, func(tx *sql.Tx, u Upload) (Upload, error) {
		if r.billingNow != nil {
			now = maxTime(now, r.billingNow().UTC())
		}
		if u.State != Verifying || u.LeaseToken != expected.LeaseToken || !validOwner(expected.LeaseToken) || !now.Before(u.LeaseUntil) || !now.Before(u.ExpiresAt) {
			return u, ErrLease
		}
		if u.Digest != expected.Digest || !sealResource(u, resource) {
			return u, ErrConflict
		}
		var digest string
		var stored sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT manifest_digest,object_version FROM album_original_seals WHERE upload_id=? AND resource_id=? FOR UPDATE`, u.ID, resource).Scan(&digest, &stored)
		if err == nil {
			if digest != u.Digest || stored.Valid && !concreteCOSVersion(stored.String) {
				return u, ErrConflict
			}
			version = stored.String
			return u, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return u, err
		}
		if err := requireActiveUpload(ctx, tx, u); err != nil {
			return u, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO album_original_seals(upload_id,resource_id,manifest_digest,consumed_at) VALUES(?,?,?,?)`, u.ID, resource, u.Digest, now)
		fresh = err == nil
		return u, err
	})
	if err != nil {
		return "", false, err
	}
	return version, fresh, nil
}

// Retain positive late receipts even after lease expiry or logical trash. This
// records an immutable version; it neither publishes a backup nor grants writes.
func (r *MySQLUploads) RecordOriginalSeal(ctx context.Context, expected Upload, resource, version string) error {
	if !validOwner(expected.OwnerID) || !validOwner(expected.ID) || !sealResource(expected, resource) || !concreteCOSVersion(version) {
		return ErrInvalidManifest
	}
	_, err := r.mutate(ctx, expected.OwnerID, expected.ID, func(tx *sql.Tx, u Upload) (Upload, error) {
		if u.Digest != expected.Digest || !sealResource(u, resource) {
			return u, ErrConflict
		}
		var digest string
		var stored sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT manifest_digest,object_version FROM album_original_seals WHERE upload_id=? AND resource_id=? FOR UPDATE`, u.ID, resource).Scan(&digest, &stored)
		if errors.Is(err, sql.ErrNoRows) {
			return u, ErrConflict
		}
		if err != nil {
			return u, err
		}
		if digest != u.Digest || stored.Valid && stored.String != version {
			return u, ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE album_original_seals SET object_version=? WHERE upload_id=? AND resource_id=?`, version, u.ID, resource)
		return u, err
	})
	return err
}

func closedOriginalSeals(ctx context.Context, tx *sql.Tx, u Upload) error {
	rows, err := tx.QueryContext(ctx, `SELECT resource_id,manifest_digest,object_version,legacy FROM album_original_seals WHERE upload_id=?`, u.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var resource, digest string
		var version sql.NullString
		var legacy bool
		if err := rows.Scan(&resource, &digest, &version, &legacy); err != nil {
			return err
		}
		if legacy || digest != u.Digest || !version.Valid || !concreteCOSVersion(version.String) {
			return ErrDerivedPending
		}
		matched := false
		for _, object := range u.Objects {
			if object.ResourceID == resource && object.VersionID == version.String {
				matched = true
				break
			}
		}
		if !matched {
			return ErrConflict
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != len(u.Manifest.Resources) {
		return ErrDerivedPending
	}
	return nil
}
