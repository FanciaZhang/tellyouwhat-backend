package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type LibraryRepository interface {
	AssetVersion(context.Context, string, string) (AssetVersion, error)
	AssetVersions(context.Context, string, int64, int) (AssetVersionPage, error)
}

// AssetVersion is an immutable version, not a mutable current-album item.
// Object locations remain server-only; clients will obtain scoped read grants.
type AssetVersion struct {
	Sequence       int64          `json:"sequence"`
	AssetID        string         `json:"assetID"`
	VersionID      string         `json:"versionID"`
	ManifestDigest string         `json:"manifestDigest"`
	Manifest       Manifest       `json:"manifest"`
	OriginalBytes  int64          `json:"originalBytes"`
	PublishedAt    time.Time      `json:"publishedAt"`
	Objects        []SealedObject `json:"-"`
}
type AssetVersionPage struct {
	Versions     []AssetVersion `json:"versions"`
	NextSequence int64          `json:"nextSequence"`
	HasMore      bool           `json:"hasMore"`
}

const versionColumns = `sequence_id,asset_id,version_id,manifest_digest,manifest_json,original_bytes,published_at,sealed_objects`

func scanAssetVersion(row rowScanner) (AssetVersion, error) {
	var v AssetVersion
	var manifest, objects []byte
	err := row.Scan(&v.Sequence, &v.AssetID, &v.VersionID, &v.ManifestDigest, &manifest, &v.OriginalBytes, &v.PublishedAt, &objects)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(manifest, &v.Manifest); err != nil {
		return v, err
	}
	if err := json.Unmarshal(objects, &v.Objects); err != nil {
		return v, err
	}
	if v.Sequence <= 0 || v.Manifest.AssetID != v.AssetID {
		return v, ErrIncompleteBackup
	}
	u := Upload{ID: v.VersionID, Manifest: v.Manifest, Digest: v.ManifestDigest, OriginalBytes: v.OriginalBytes}
	// Caller supplies owner for key-scope validation; this scanner validates the
	// shape and complete byte evidence without exposing provider locations.
	observations := make([]Observation, 0, len(v.Objects))
	var total int64
	for _, object := range v.Objects {
		if object.VersionID == "" {
			return v, ErrIncompleteBackup
		}
		observations = append(observations, Observation{object.ResourceID, object.SizeBytes, object.SHA256})
	}
	digest, err := VerifyBackup(u.Manifest, observations)
	if err != nil || digest != u.Digest {
		return v, ErrIncompleteBackup
	}
	for _, r := range v.Manifest.Resources {
		total += r.SizeBytes
	}
	if total != v.OriginalBytes {
		return v, ErrIncompleteBackup
	}
	return v, nil
}
func validateVersionOwner(v AssetVersion, owner string) error {
	if !validOwner(v.VersionID) {
		return ErrIncompleteBackup
	}
	u := Upload{ID: v.VersionID, OwnerID: owner}
	for _, object := range v.Objects {
		if object.Key != sealedKey(u, object.ResourceID) {
			return ErrIncompleteBackup
		}
	}
	return nil
}

// publishAssetVersion must be called while mutate holds the account lock and
// after byte verification. Both quota and upload state roll back if this fails.
func publishAssetVersion(ctx context.Context, tx *sql.Tx, u Upload, objects []SealedObject, now time.Time) error {
	observations := make([]Observation, 0, len(objects))
	for _, object := range objects {
		if object.Key != sealedKey(u, object.ResourceID) || object.VersionID == "" {
			return ErrIncompleteBackup
		}
		observations = append(observations, Observation{object.ResourceID, object.SizeBytes, object.SHA256})
	}
	digest, err := VerifyBackup(u.Manifest, observations)
	if err != nil || digest != u.Digest {
		return ErrIncompleteBackup
	}
	existing, err := scanAssetVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND version_id=?`, u.OwnerID, u.ID))
	if err == nil {
		if existing.ManifestDigest != u.Digest || existing.OriginalBytes != u.OriginalBytes {
			return ErrConflict
		}
		if err := validateVersionOwner(existing, u.OwnerID); err != nil {
			return err
		}
		// Provider versions must not be replaced by an idempotent replay.
		expected := make(map[string]SealedObject, len(objects))
		for _, object := range objects {
			expected[object.ResourceID] = object
		}
		for _, object := range existing.Objects {
			if expected[object.ResourceID] != object {
				return ErrConflict
			}
		}
		return enqueuePreviewJobs(ctx, tx, u, now)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	manifest, err := json.Marshal(u.Manifest)
	if err != nil {
		return err
	}
	sealed, err := json.Marshal(objects)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO album_asset_versions(owner_id,asset_id,version_id,manifest_digest,manifest_json,sealed_objects,original_bytes,published_at) VALUES(?,?,?,?,?,?,?,?)`,
		u.OwnerID, u.Manifest.AssetID, u.ID, u.Digest, manifest, sealed, u.OriginalBytes, now)
	if err != nil {
		return err
	}
	return enqueuePreviewJobs(ctx, tx, u, now)
}
func (r *MySQLUploads) CheckLibrarySchema(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions LIMIT 0`)
	if err != nil {
		return err
	}
	return rows.Close()
}
func (r *MySQLUploads) AssetVersion(ctx context.Context, owner, versionID string) (AssetVersion, error) {
	if !validOwner(owner) {
		return AssetVersion{}, ErrOwner
	}
	if !validOwner(versionID) {
		return AssetVersion{}, ErrInvalidManifest
	}
	result, err := scanAssetVersion(r.db.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND version_id=?`, owner, versionID))
	if err != nil {
		return result, err
	}
	return result, validateVersionOwner(result, owner)
}
func (r *MySQLUploads) AssetVersions(ctx context.Context, owner string, after int64, limit int) (AssetVersionPage, error) {
	result := AssetVersionPage{Versions: []AssetVersion{}, NextSequence: after}
	if !validOwner(owner) {
		return result, ErrOwner
	}
	if after < 0 || limit < 1 || limit > 100 {
		return result, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND sequence_id>? ORDER BY sequence_id LIMIT ?`, owner, after, limit+1)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	encodedBytes := 0
	for rows.Next() {
		v, err := scanAssetVersion(rows)
		if err != nil {
			return result, err
		}
		if err := validateVersionOwner(v, owner); err != nil {
			return result, err
		}
		if len(result.Versions) == limit {
			result.HasMore = true
			break
		}
		data, err := json.Marshal(v)
		if err != nil {
			return result, err
		}
		if len(data) > (1<<20)-4096 {
			return result, ErrInvalidManifest
		}
		if encodedBytes+len(data) > (1<<20)-4096 {
			result.HasMore = true
			break
		}
		encodedBytes += len(data)
		result.Versions = append(result.Versions, v)
		result.NextSequence = v.Sequence
	}
	return result, rows.Err()
}
