package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

// Only the immutable 30-day trash deadline qualifies here. Subscription expiry
// requires a separate current-Apple-evidence cutover and cannot call this path.
func (r *MySQLUploads) prepareExpiredTrashPurge(ctx context.Context, owner, asset string, now time.Time) (objectPurge, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return objectPurge{}, err
	}
	defer tx.Rollback()
	a, err := lockExpiredTrash(ctx, tx, owner, asset, now)
	if err != nil {
		return objectPurge{}, err
	}
	old, err := scanObjectPurge(tx.QueryRowContext(ctx, `SELECT `+objectPurgeColumns+` FROM album_object_purges WHERE owner_id=? AND asset_id=? FOR UPDATE`, owner, asset))
	if err == nil {
		if old.Reason != "trash" || old.Revision != a.Revision || !old.ExpiresAt.Equal(*a.TrashExpiresAt) {
			return old, ErrConflict
		}
		return old, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return objectPurge{}, err
	}
	targets, err := closedPurgeTargets(ctx, tx, owner, asset, now)
	if err != nil {
		return objectPurge{}, err
	}
	p := objectPurge{Reason: "trash", OwnerID: owner, AssetID: asset, Revision: a.Revision, ExpiresAt: *a.TrashExpiresAt, Targets: targets, State: "queued"}
	if p.validate() != nil {
		return p, ErrConflict
	}
	data, err := json.Marshal(targets)
	if err != nil || len(data) > 4<<20 {
		return p, ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO album_object_purges(owner_id,asset_id,revision,trash_expires_at,targets_json,available_at,updated_at) VALUES(?,?,?,?,?,?,?)`, owner, asset, p.Revision, p.ExpiresAt, data, now, now)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

// A timeout is not a terminal provider receipt. Defer any unresolved write,
// open escrow or retirement; these can still create objects or change billing.
func closedPurgeTargets(ctx context.Context, tx *sql.Tx, owner, asset string, now time.Time) ([]purgeTarget, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions WHERE owner_id=? AND asset_id=? ORDER BY version_id LIMIT 1001`, owner, asset)
	if err != nil {
		return nil, err
	}
	var versions []AssetVersion
	for rows.Next() {
		v, err := scanAssetVersion(rows)
		if err != nil || validateVersionOwner(v, owner) != nil {
			rows.Close()
			return nil, ErrIncompleteBackup
		}
		versions = append(versions, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// shortcut: larger asset histories defer intact; add paged admission before
	// enabling accounts with more than 1000 revisions of one logical asset.
	if len(versions) == 0 || len(versions) > 1000 {
		return nil, ErrConflict
	}
	var uploads int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_uploads WHERE owner_id=? AND JSON_UNQUOTE(JSON_EXTRACT(manifest_json,'$.assetID'))=?`, owner, asset).Scan(&uploads); err != nil {
		return nil, err
	}
	if uploads != len(versions) {
		return nil, ErrDerivedPending
	}
	var targets []purgeTarget
	for _, v := range versions {
		u, err := scanUpload(tx.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM album_uploads WHERE owner_id=? AND upload_id=?`, owner, v.VersionID))
		if err != nil {
			return nil, err
		}
		if u.State != OriginalsVerified || u.Manifest.AssetID != asset || u.Digest != v.ManifestDigest || u.ExpiresAt.After(now) {
			return nil, ErrDerivedPending
		}
		if err := closedOriginalSeals(ctx, tx, u); err != nil {
			return nil, err
		}
		f, err := scanFinalization(tx.QueryRowContext(ctx, `SELECT `+finalizationColumns+` FROM album_derived_finalizations WHERE owner_id=? AND version_id=?`, owner, v.VersionID))
		if errors.Is(err, ErrNotFound) {
			return nil, ErrDerivedPending
		}
		if err != nil {
			return nil, err
		}
		if f.ManifestDigest != v.ManifestDigest || f.HeldBytes != 0 {
			return nil, ErrDerivedPending
		}
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM album_uploads WHERE upload_id=? AND derived_budget_closed=FALSE)+
 (SELECT COUNT(*) FROM album_multipart_intents i LEFT JOIN album_multipart_completions c ON c.upload_id=i.upload_id AND c.resource_id=i.resource_id WHERE i.upload_id=? AND (i.state<>'ready' OR c.state IS NULL OR c.state<>'assembled'))+
 (SELECT COUNT(*) FROM album_photo_jobs WHERE owner_id=? AND version_id=? AND state NOT IN ('completed','attention'))+
 (SELECT COUNT(*) FROM album_derived_cleanups WHERE owner_id=? AND version_id=? AND state<>'completed')`, u.ID, u.ID, owner, u.ID, owner, u.ID).Scan(&pending); err != nil {
			return nil, err
		}
		if pending != 0 {
			return nil, ErrDerivedPending
		}
		for _, resource := range v.Manifest.Resources {
			targets = append(targets, purgeTarget{stagingKey(u, resource.ID)}, purgeTarget{sealedKey(u, resource.ID)})
		}
		more, err := closedDerivedPurgeTargets(ctx, tx, owner, v, now)
		if err != nil {
			return nil, err
		}
		targets = append(targets, more...)
		if len(targets) > 10000 {
			return nil, ErrConflict
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Key < targets[j].Key })
	return targets, nil
}

func closedDerivedPurgeTargets(ctx context.Context, tx *sql.Tx, owner string, v AssetVersion, now time.Time) ([]purgeTarget, error) {
	var targets []purgeTarget
	rows, err := tx.QueryContext(ctx, `SELECT `+photoWriteColumns+` FROM album_photo_writes WHERE owner_id=? AND version_id=?`, owner, v.VersionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		p, err := scanPhotoWrite(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if p.ManifestDigest != v.ManifestDigest || p.ObjectVersion == "" && p.RetiredAt.IsZero() {
			rows.Close()
			return nil, ErrDerivedPending
		}
		key, err := p.Spec.key()
		if err != nil {
			rows.Close()
			return nil, err
		}
		targets = append(targets, purgeTarget{key})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT `+videoJobColumns+` FROM album_video_jobs WHERE owner_id=? AND version_id=?`, owner, v.VersionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		j, err := scanVideoJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if j.ManifestDigest != v.ManifestDigest || (j.State != "completed" && j.State != "attention") || (j.Spec != nil && j.Result == nil && j.RetiredAt.IsZero()) {
			rows.Close()
			return nil, ErrDerivedPending
		}
		if j.Spec != nil {
			key, err := (DerivedCleanupTarget{"video", owner, j.ID, string(j.Preset)}).key()
			if err != nil {
				rows.Close()
				return nil, err
			}
			targets = append(targets, purgeTarget{key})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT `+livePairColumns+` FROM album_live_pairs WHERE owner_id=? AND version_id=?`, owner, v.VersionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		p, err := scanLivePair(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if p.AssetID != v.AssetID || p.ManifestDigest != v.ManifestDigest || (p.State != "verified" && p.State != "retired") || p.ExpiresAt.After(now) {
			rows.Close()
			return nil, ErrDerivedPending
		}
		for _, id := range []string{"photo", "motion"} {
			for _, staging := range []bool{false, true} {
				key, err := p.key(id, staging)
				if err != nil {
					rows.Close()
					return nil, err
				}
				targets = append(targets, purgeTarget{key})
			}
		}
	}
	err = rows.Err()
	rows.Close()
	return targets, err
}
