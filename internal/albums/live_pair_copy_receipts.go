package albums

import (
	"context"
	"database/sql"
)

// Preserve a positive COPY result even after cancellation, lease expiry, trash,
// or deletion acceptance. Byte verification and quota settlement remain separate.
func (r *MySQLUploads) RecordLivePairSeal(ctx context.Context, expected LivePairUpload, part, version string) (LivePairUpload, error) {
	if !concreteCOSVersion(version) {
		return LivePairUpload{}, ErrInvalidManifest
	}
	if _, err := expected.resource(part); err != nil {
		return LivePairUpload{}, err
	}
	return r.mutateLivePair(ctx, expected.OwnerID, expected.ID, func(tx *sql.Tx, current LivePairUpload) (LivePairUpload, error) {
		return recordLivePairSeal(ctx, tx, current, expected, part, version)
	})
}

func recordLivePairSeal(ctx context.Context, tx *sql.Tx, current, expected LivePairUpload, part, version string) (LivePairUpload, error) {
	if !sameLivePairCopy(current, expected) {
		return current, ErrConflict
	}
	column, stored, consumed := "photo_copy_version", current.PhotoCopyVersion, current.PhotoConsumed
	if part == "motion" {
		column, stored, consumed = "movie_copy_version", current.MovieCopyVersion, current.MovieConsumed
	} else if part != "photo" {
		return current, ErrInvalidManifest
	}
	if !consumed || !concreteCOSVersion(version) || stored != "" && stored != version {
		return current, ErrConflict
	}
	if stored != "" {
		return current, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_live_pairs SET `+column+`=? WHERE pair_upload_id=?`, version, current.ID); err != nil {
		return current, err
	}
	if part == "photo" {
		current.PhotoCopyVersion = version
	} else {
		current.MovieCopyVersion = version
	}
	return current, nil
}

func sameLivePairCopy(a, b LivePairUpload) bool {
	return a.ID == b.ID && a.OwnerID == b.OwnerID && a.VersionID == b.VersionID && a.AssetID == b.AssetID && a.ManifestDigest == b.ManifestDigest && a.Descriptor == b.Descriptor && a.SourcePhoto == b.SourcePhoto && a.SourceVideo == b.SourceVideo
}
