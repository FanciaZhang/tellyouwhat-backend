package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const objectPurgeColumns = `owner_id,asset_id,revision,trash_expires_at,targets_json,target_index,plan_json,action_index,state,lease_token,lease_until`

func scanObjectPurge(row rowScanner) (objectPurge, error) {
	var p objectPurge
	var targets, plan []byte
	var token sql.NullString
	var until sql.NullTime
	err := row.Scan(&p.OwnerID, &p.AssetID, &p.Revision, &p.ExpiresAt, &targets, &p.Index, &plan, &p.ActionIndex, &p.State, &token, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.LeaseToken, p.LeaseUntil = token.String, until.Time
	if len(targets) > 4<<20 || json.Unmarshal(targets, &p.Targets) != nil || len(plan) > 2<<20 {
		return p, ErrConflict
	}
	if len(plan) > 0 {
		p.Plan = &purgePlan{}
		if json.Unmarshal(plan, p.Plan) != nil {
			return p, ErrConflict
		}
	}
	return p, p.validate()
}

func (r *MySQLUploads) objectPurge(ctx context.Context, owner, asset string) (objectPurge, error) {
	if !validOwner(owner) || !resourceIDPattern.MatchString(asset) {
		return objectPurge{}, ErrInvalidManifest
	}
	return scanObjectPurge(r.db.QueryRowContext(ctx, `SELECT `+objectPurgeColumns+` FROM album_object_purges WHERE owner_id=? AND asset_id=?`, owner, asset))
}

func lockExpiredTrash(ctx context.Context, tx *sql.Tx, owner, asset string, now time.Time) (CloudAssetState, error) {
	if !validOwner(owner) || !resourceIDPattern.MatchString(asset) || now.IsZero() {
		return CloudAssetState{}, ErrInvalidManifest
	}
	if _, err := lockAccount(ctx, tx, owner); err != nil {
		return CloudAssetState{}, err
	}
	a, err := scanAssetState(tx.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM album_asset_lifecycle WHERE owner_id=? AND asset_id=? FOR UPDATE`, owner, asset))
	if err != nil {
		return a, err
	}
	if a.State != "trashed" || a.Revision <= 0 || a.TrashExpiresAt == nil || a.TrashExpiresAt.After(now) {
		return a, ErrConflict
	}
	return a, nil
}

func (r *MySQLUploads) lockObjectPurge(ctx context.Context, tx *sql.Tx, owner, asset string, now time.Time) (objectPurge, error) {
	a, err := lockExpiredTrash(ctx, tx, owner, asset, now)
	if err != nil {
		return objectPurge{}, err
	}
	p, err := scanObjectPurge(tx.QueryRowContext(ctx, `SELECT `+objectPurgeColumns+` FROM album_object_purges WHERE owner_id=? AND asset_id=? FOR UPDATE`, owner, asset))
	if err != nil {
		return p, err
	}
	if p.Revision != a.Revision || !p.ExpiresAt.Equal(*a.TrashExpiresAt) {
		return p, ErrConflict
	}
	return p, nil
}

func (r *MySQLUploads) claimObjectPurge(ctx context.Context, owner, asset, token string, now time.Time) (objectPurge, error) {
	if !validOwner(token) {
		return objectPurge{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return objectPurge{}, err
	}
	defer tx.Rollback()
	p, err := r.lockObjectPurge(ctx, tx, owner, asset, now)
	if err != nil {
		return p, err
	}
	var available time.Time
	if err := tx.QueryRowContext(ctx, `SELECT available_at FROM album_object_purges WHERE owner_id=? AND asset_id=?`, owner, asset).Scan(&available); err != nil {
		return p, err
	}
	if p.State == "objects_removed" || available.After(now) || p.State == "running" && p.LeaseUntil.After(now) {
		return p, ErrLease
	}
	p.State, p.LeaseToken, p.LeaseUntil = "running", token, now.Add(10*time.Minute)
	_, err = tx.ExecContext(ctx, `UPDATE album_object_purges SET state='running',lease_token=?,lease_until=?,updated_at=? WHERE owner_id=? AND asset_id=?`, token, p.LeaseUntil, now, owner, asset)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func (r *MySQLUploads) checkObjectPurgeLease(ctx context.Context, expected objectPurge, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := r.lockObjectPurge(ctx, tx, expected.OwnerID, expected.AssetID, now)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(p.Plan)
	b, _ := json.Marshal(expected.Plan)
	if !samePurgeLease(p, expected, now) || p.Plan == nil || string(a) != string(b) {
		return ErrLease
	}
	return tx.Commit()
}

func (r *MySQLUploads) storeObjectPurgePlan(ctx context.Context, expected objectPurge, plan purgePlan, now time.Time) (objectPurge, error) {
	data, err := json.Marshal(plan)
	if err != nil || len(data) > 2<<20 || plan.validate() != nil {
		return objectPurge{}, ErrConflict
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return objectPurge{}, err
	}
	defer tx.Rollback()
	p, err := r.lockObjectPurge(ctx, tx, expected.OwnerID, expected.AssetID, now)
	if err != nil {
		return p, err
	}
	if !samePurgeLease(p, expected, now) {
		return p, ErrLease
	}
	if p.Plan != nil {
		old, _ := json.Marshal(p.Plan)
		if string(old) != string(data) {
			return p, ErrConflict
		}
		return p, tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `UPDATE album_object_purges SET plan_json=?,updated_at=? WHERE owner_id=? AND asset_id=?`, data, now, p.OwnerID, p.AssetID)
	if err != nil {
		return p, err
	}
	p.Plan = &plan
	return p, tx.Commit()
}

func (r *MySQLUploads) finishObjectPurgeStep(ctx context.Context, expected objectPurge, empty bool, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := r.lockObjectPurge(ctx, tx, expected.OwnerID, expected.AssetID, now)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(p.Plan)
	b, _ := json.Marshal(expected.Plan)
	if !samePurgeLease(p, expected, now) || p.Plan == nil || string(a) != string(b) {
		return ErrLease
	}
	if p.ActionIndex != len(p.Plan.Versions)+len(p.Plan.Uploads) {
		return ErrConflict
	}
	if empty && len(p.Plan.Versions) == 0 && len(p.Plan.Uploads) == 0 {
		p.Index++
	}
	state := "queued"
	if p.Index == len(p.Targets) {
		state = "objects_removed"
	}
	_, err = tx.ExecContext(ctx, `UPDATE album_object_purges SET state=?,target_index=?,plan_json=NULL,action_index=0,lease_token=NULL,lease_until=NULL,available_at=?,updated_at=? WHERE owner_id=? AND asset_id=?`, state, p.Index, now, now, p.OwnerID, p.AssetID)
	if err != nil {
		return err
	}
	// No quota refund or deletion receipt: objects_removed is only an observation,
	// not completion of metadata, backup or account erasure.
	return tx.Commit()
}

// Acknowledged actions advance durably, so a large batch can span leases.
// A lost response or failed commit retries only the unacknowledged exact target.
func (r *MySQLUploads) ackObjectPurgeAction(ctx context.Context, expected objectPurge, now time.Time) (objectPurge, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return objectPurge{}, err
	}
	defer tx.Rollback()
	p, err := r.lockObjectPurge(ctx, tx, expected.OwnerID, expected.AssetID, now)
	if err != nil {
		return p, err
	}
	a, _ := json.Marshal(p.Plan)
	b, _ := json.Marshal(expected.Plan)
	if !samePurgeLease(p, expected, now) || p.Plan == nil || string(a) != string(b) {
		return p, ErrLease
	}
	if p.ActionIndex >= len(p.Plan.Versions)+len(p.Plan.Uploads) {
		return p, ErrConflict
	}
	p.ActionIndex++
	_, err = tx.ExecContext(ctx, `UPDATE album_object_purges SET action_index=?,updated_at=? WHERE owner_id=? AND asset_id=?`, p.ActionIndex, now, p.OwnerID, p.AssetID)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}
