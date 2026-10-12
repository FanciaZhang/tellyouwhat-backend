package albums

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

// No scheduler calls this yet. Complete object removal still needs provider
// quiescence and backup deletion propagation before production activation.
func (r *MySQLUploads) settleObjectPurge(ctx context.Context, owner, asset string, storage objectPurgeStorage, now func() time.Time) (int64, error) {
	if storage == nil || now == nil {
		return 0, ErrInvalidManifest
	}
	expected, err := r.objectPurge(ctx, owner, asset)
	if err != nil {
		return 0, err
	}
	if expected.State != "completed" {
		if expected.State != "objects_removed" {
			return 0, ErrConflict
		}
		for _, target := range expected.Targets {
			uploads, err := storage.listPurgeUploads(ctx, owner, target)
			if err != nil {
				return 0, err
			}
			versions, err := storage.listPurgeVersions(ctx, owner, target)
			if err != nil {
				return 0, err
			}
			if len(uploads) != 0 || len(versions) != 0 {
				if err := r.requeueObjectPurge(ctx, expected, now().UTC()); err != nil {
					return 0, err
				}
				return 0, ErrDerivedPending
			}
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	balance, err := lockAccount(ctx, tx, owner)
	if err != nil {
		return 0, err
	}
	p, err := scanObjectPurge(tx.QueryRowContext(ctx, `SELECT `+objectPurgeColumns+` FROM album_object_purges WHERE owner_id=? AND asset_id=? FOR UPDATE`, owner, asset))
	if err != nil {
		return 0, err
	}
	if p.State == "completed" {
		var refund int64
		if err := tx.QueryRowContext(ctx, `SELECT refunded_bytes FROM album_object_purges WHERE owner_id=? AND asset_id=?`, owner, asset).Scan(&refund); err != nil {
			return 0, err
		}
		return refund, tx.Commit()
	}
	clock := now().UTC()
	a, err := r.validatePurgeLifecycle(ctx, tx, p, clock)
	if err != nil {
		return 0, err
	}
	x, _ := json.Marshal(p.Targets)
	y, _ := json.Marshal(expected.Targets)
	if p.State != "objects_removed" || p.Revision != expected.Revision || p.Revision != a.Revision || string(x) != string(y) || a.Revision == math.MaxInt64 || a.epoch == math.MaxInt64 {
		return 0, ErrConflict
	}
	targets, err := closedPurgeTargets(ctx, tx, owner, asset, clock)
	if err != nil {
		return 0, err
	}
	z, _ := json.Marshal(targets)
	if string(x) != string(z) {
		return 0, ErrConflict
	}
	var refund int64
	err = tx.QueryRowContext(ctx, `SELECT
 COALESCE((SELECT SUM(v.original_bytes+f.retained_bytes) FROM album_asset_versions v JOIN album_derived_finalizations f ON f.owner_id=v.owner_id AND f.version_id=v.version_id WHERE v.owner_id=? AND v.asset_id=?),0)+
 COALESCE((SELECT SUM(p.size_bytes) FROM album_live_pairs p JOIN album_asset_versions v ON v.owner_id=p.owner_id AND v.version_id=p.version_id WHERE v.owner_id=? AND v.asset_id=? AND p.state='verified'),0)`, owner, asset, owner, asset).Scan(&refund)
	if err != nil {
		return 0, err
	}
	if refund <= 0 || balance.used < refund {
		return 0, ErrQuota
	}
	// Remove old active events with their manifests, retaining only opaque IDs
	// and a final revision so old cursors and fresh installations converge.
	if _, err = tx.ExecContext(ctx, `DELETE FROM album_library_changes WHERE owner_id=? AND asset_id=?`, owner, asset); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO album_library_changes(owner_id,asset_id,version_id,revision,state,changed_at) SELECT owner_id,asset_id,version_id,?,'purged',? FROM album_asset_versions WHERE owner_id=? AND asset_id=? ORDER BY sequence_id`, a.Revision+1, clock, owner, asset)
	if err != nil {
		return 0, err
	}
	if n, e := result.RowsAffected(); e != nil || n == 0 {
		return 0, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `DELETE c FROM album_share_claims c JOIN album_share_links s ON s.share_id=c.share_id JOIN album_asset_versions v ON v.owner_id=s.owner_id AND v.version_id=s.version_id WHERE v.owner_id=? AND v.asset_id=?`, owner, asset); err != nil {
		return 0, err
	}
	for _, table := range []string{"album_share_links", "album_derived_cleanups", "album_derived_finalizations", "album_photo_writes", "album_photo_jobs", "album_video_jobs", "album_live_pairs"} {
		if _, err = tx.ExecContext(ctx, `DELETE c FROM `+table+` c JOIN album_asset_versions v ON v.owner_id=c.owner_id AND v.version_id=c.version_id WHERE v.owner_id=? AND v.asset_id=?`, owner, asset); err != nil {
			return 0, err
		}
	}
	for _, table := range []string{"album_multipart_completions", "album_multipart_intents", "album_uploads"} {
		if _, err = tx.ExecContext(ctx, `DELETE c FROM `+table+` c JOIN album_asset_versions v ON v.version_id=c.upload_id WHERE v.owner_id=? AND v.asset_id=?`, owner, asset); err != nil {
			return 0, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM album_asset_versions WHERE owner_id=? AND asset_id=?`, owner, asset); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM album_asset_operations WHERE owner_id=? AND asset_id=?`, owner, asset); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_asset_lifecycle SET state='purged',revision=revision+1,lifecycle_epoch=lifecycle_epoch+1,trashed_at=NULL,trash_expires_at=NULL WHERE owner_id=? AND asset_id=?`, owner, asset); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=used_bytes-? WHERE owner_id=?`, refund, owner); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_object_purges SET state='completed',targets_json=JSON_ARRAY(),target_index=0,refunded_bytes=?,completed_at=?,updated_at=? WHERE owner_id=? AND asset_id=?`, refund, clock, clock, owner, asset); err != nil {
		return 0, err
	}
	if p.Reason == "retention" && p.StartedAt.IsZero() && !now().UTC().Before(p.VerificationUntil) {
		return 0, ErrLease
	}
	return refund, tx.Commit()
}

func (r *MySQLUploads) requeueObjectPurge(ctx context.Context, expected objectPurge, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := r.lockObjectPurge(ctx, tx, expected.OwnerID, expected.AssetID, now)
	if err != nil {
		return err
	}
	x, _ := json.Marshal(p.Targets)
	y, _ := json.Marshal(expected.Targets)
	if p.State != "objects_removed" || p.Revision != expected.Revision || string(x) != string(y) {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_object_purges SET state='queued',target_index=0,available_at=?,updated_at=? WHERE owner_id=? AND asset_id=?`, now, now, p.OwnerID, p.AssetID); err != nil {
		return err
	}
	return tx.Commit()
}
