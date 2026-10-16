package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"
)

func (r *MySQLUploads) prepareRetentionObjectPurge(ctx context.Context, proof expiredRetentionProof, asset string, now time.Time) (objectPurge, error) {
	if !resourceIDPattern.MatchString(asset) {
		return objectPurge{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return objectPurge{}, err
	}
	defer tx.Rollback()
	if _, err = lockAccount(ctx, tx, proof.owner); err != nil {
		return objectPurge{}, err
	}
	if r.billingNow != nil {
		now = maxTime(now, r.billingNow().UTC())
	}
	if err := r.checkExpiredRetention(ctx, tx, proof, now); err != nil {
		return objectPurge{}, err
	}
	a, err := scanAssetState(tx.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM album_asset_lifecycle WHERE owner_id=? AND asset_id=? FOR UPDATE`, proof.owner, asset))
	if err != nil {
		return objectPurge{}, err
	}
	if a.Revision < 1 || (a.State != "active" && a.State != "trashed") {
		return objectPurge{}, ErrConflict
	}
	old, err := scanObjectPurge(tx.QueryRowContext(ctx, `SELECT `+objectPurgeColumns+` FROM album_object_purges WHERE owner_id=? AND asset_id=? FOR UPDATE`, proof.owner, asset))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return objectPurge{}, err
	}
	if err == nil && (old.Reason != "retention" || !old.StartedAt.IsZero()) {
		return objectPurge{}, ErrConflict
	}
	targets, err := closedPurgeTargets(ctx, tx, proof.owner, asset, now)
	if err != nil {
		return objectPurge{}, err
	}
	p := objectPurge{OwnerID: proof.owner, AssetID: asset, Revision: a.Revision, ExpiresAt: proof.deadline, State: "queued", Targets: targets,
		Reason: "retention", Environment: proof.environment, SubscriptionRevision: proof.revision, VerificationUntil: proof.until}
	if p.validate() != nil {
		return p, ErrConflict
	}
	data, err := json.Marshal(targets)
	if err != nil || len(data) > 4<<20 {
		return p, ErrConflict
	}
	// No provider mutation has been authorized; a fresh proof may replace an
	// expired plan and its lease without discarding an external outcome.
	_, err = tx.ExecContext(ctx, `INSERT INTO album_object_purges(owner_id,asset_id,revision,trash_expires_at,targets_json,available_at,updated_at,reason,environment,subscription_revision,verification_until) VALUES(?,?,?,?,?,?,?,'retention',?,?,?)
 ON DUPLICATE KEY UPDATE revision=VALUES(revision),trash_expires_at=VALUES(trash_expires_at),targets_json=VALUES(targets_json),target_index=0,action_index=0,plan_json=NULL,state='queued',lease_token=NULL,lease_until=NULL,available_at=VALUES(available_at),updated_at=VALUES(updated_at),environment=VALUES(environment),subscription_revision=VALUES(subscription_revision),verification_until=VALUES(verification_until)`, proof.owner, asset, p.Revision, p.ExpiresAt, data, now, now, p.Environment, p.SubscriptionRevision, p.VerificationUntil)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (r *MySQLUploads) validatePurgeLifecycle(ctx context.Context, tx *sql.Tx, p objectPurge, now time.Time) (CloudAssetState, error) {
	a, err := scanAssetState(tx.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM album_asset_lifecycle WHERE owner_id=? AND asset_id=? FOR UPDATE`, p.OwnerID, p.AssetID))
	if err != nil {
		return a, err
	}
	if a.Revision != p.Revision {
		return a, ErrConflict
	}
	if p.Reason == "trash" {
		if a.State != "trashed" || a.TrashExpiresAt == nil || !p.ExpiresAt.Equal(*a.TrashExpiresAt) || p.ExpiresAt.After(now) {
			return a, ErrConflict
		}
		return a, nil
	}
	if p.Environment != r.billingEnvironment {
		return a, ErrLease
	}
	if !p.StartedAt.IsZero() {
		if a.State != "purging" {
			return a, ErrConflict
		}
		return a, nil
	}
	if a.State != "active" && a.State != "trashed" {
		return a, ErrConflict
	}
	if r.billingNow != nil {
		now = maxTime(now, r.billingNow().UTC())
	}
	err = r.checkExpiredRetention(ctx, tx, expiredRetentionProof{p.OwnerID, p.Environment, p.SubscriptionRevision, p.ExpiresAt, p.VerificationUntil}, now)
	return a, err
}

// This durable grant is the irreversible boundary, immediately before the
// first provider request. Renewal cancels only assets that have not crossed it.
func (r *MySQLUploads) authorizeObjectPurgeAction(ctx context.Context, expected objectPurge, now func() time.Time) (objectPurge, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return objectPurge{}, err
	}
	defer tx.Rollback()
	p, err := r.lockObjectPurge(ctx, tx, expected.OwnerID, expected.AssetID, now().UTC())
	if err != nil {
		return p, err
	}
	clock := now().UTC()
	x, _ := json.Marshal(p.Plan)
	y, _ := json.Marshal(expected.Plan)
	if !samePurgeLease(p, expected, clock) || p.Plan == nil || string(x) != string(y) || p.ActionIndex >= len(p.Plan.Uploads)+len(p.Plan.Versions) {
		return p, ErrLease
	}
	starting := p.Reason == "retention" && p.StartedAt.IsZero()
	if starting {
		if err := r.checkExpiredRetention(ctx, tx, expiredRetentionProof{p.OwnerID, p.Environment, p.SubscriptionRevision, p.ExpiresAt, p.VerificationUntil}, clock); err != nil {
			return p, err
		}
		a, err := scanAssetState(tx.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM album_asset_lifecycle WHERE owner_id=? AND asset_id=? FOR UPDATE`, p.OwnerID, p.AssetID))
		if err != nil {
			return p, err
		}
		if a.Revision == math.MaxInt64 || a.epoch == math.MaxInt64 {
			return p, ErrConflict
		}
		p.Revision++
		p.StartedAt = clock.Truncate(time.Microsecond)
		if _, err = tx.ExecContext(ctx, `UPDATE album_asset_lifecycle SET state='purging',revision=?,lifecycle_epoch=lifecycle_epoch+1,trashed_at=NULL,trash_expires_at=NULL WHERE owner_id=? AND asset_id=?`, p.Revision, p.OwnerID, p.AssetID); err != nil {
			return p, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM album_library_changes WHERE owner_id=? AND asset_id=?`, p.OwnerID, p.AssetID); err != nil {
			return p, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO album_library_changes(owner_id,asset_id,version_id,revision,state,changed_at) SELECT owner_id,asset_id,version_id,?,'purged',? FROM album_asset_versions WHERE owner_id=? AND asset_id=? ORDER BY sequence_id`, p.Revision, clock, p.OwnerID, p.AssetID); err != nil {
			return p, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE album_share_links s JOIN album_asset_versions v ON v.owner_id=s.owner_id AND v.version_id=s.version_id SET s.revoked_at=COALESCE(s.revoked_at,?) WHERE v.owner_id=? AND v.asset_id=?`, clock, p.OwnerID, p.AssetID); err != nil {
			return p, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM album_asset_operations WHERE owner_id=? AND asset_id=?`, p.OwnerID, p.AssetID); err != nil {
			return p, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE album_object_purges SET revision=?,started_at=?,updated_at=? WHERE owner_id=? AND asset_id=?`, p.Revision, p.StartedAt, clock, p.OwnerID, p.AssetID); err != nil {
			return p, err
		}
	}
	clock = now().UTC()
	if !clock.Before(p.LeaseUntil) || starting && !clock.Before(p.VerificationUntil) {
		return p, ErrLease
	}
	return p, tx.Commit()
}
