package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"
)

type CloudAssetState struct {
	AssetID        string     `json:"assetID"`
	Revision       int64      `json:"revision"`
	State          string     `json:"state"`
	TrashedAt      *time.Time `json:"trashedAt,omitempty"`
	TrashExpiresAt *time.Time `json:"trashExpiresAt,omitempty"`
	RestoreUntil   *time.Time `json:"restoreUntil,omitempty"`
	epoch          int64
}

type CloudAssetMutation struct {
	RequestID        string `json:"requestID"`
	ExpectedRevision int64  `json:"expectedRevision"`
}

const lifecycleColumns = `asset_id,revision,state,trashed_at,trash_expires_at,lifecycle_epoch`

func scanAssetState(row rowScanner) (CloudAssetState, error) {
	var state CloudAssetState
	var trashed, expires sql.NullTime
	err := row.Scan(&state.AssetID, &state.Revision, &state.State, &trashed, &expires, &state.epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrNotFound
	}
	if err != nil {
		return state, err
	}
	if !resourceIDPattern.MatchString(state.AssetID) || state.Revision < 0 || state.epoch < 0 || (state.State != "active" && state.State != "trashed" && state.State != "purged" && state.State != "purging") || trashed.Valid != expires.Valid || (state.State == "trashed") != trashed.Valid || expires.Valid && !trashed.Time.Before(expires.Time) {
		return state, ErrConflict
	}
	if trashed.Valid {
		state.TrashedAt = &trashed.Time
		state.TrashExpiresAt = &expires.Time
	}
	return state, nil
}

// Call only under the owner storage lock, including first upload reservation.
func ensureActiveAsset(ctx context.Context, tx *sql.Tx, owner, asset string) (CloudAssetState, error) {
	if err := requireMediaAccount(ctx, tx, owner); err != nil {
		return CloudAssetState{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO album_asset_lifecycle(owner_id,asset_id) VALUES(?,?) ON DUPLICATE KEY UPDATE asset_id=asset_id`, owner, asset); err != nil {
		return CloudAssetState{}, err
	}
	state, err := scanAssetState(tx.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM album_asset_lifecycle WHERE owner_id=? AND asset_id=? FOR UPDATE`, owner, asset))
	if err != nil {
		return state, err
	}
	if state.State != "active" {
		return state, ErrConflict
	}
	return state, nil
}

func requireVisibleVersion(ctx context.Context, tx *sql.Tx, owner, version string) error {
	if err := requireMediaAccount(ctx, tx, owner); err != nil {
		return err
	}
	var state string
	err := tx.QueryRowContext(ctx, `SELECT a.state FROM album_asset_lifecycle a JOIN album_asset_versions v ON v.owner_id=a.owner_id AND v.asset_id=a.asset_id WHERE v.owner_id=? AND v.version_id=?`, owner, version).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || err == nil && state != "active" {
		return ErrNotFound
	}
	return err
}

func (r *MySQLUploads) CloudAsset(ctx context.Context, owner, asset string, now time.Time) (CloudAssetState, error) {
	if !validOwner(owner) || !resourceIDPattern.MatchString(asset) {
		return CloudAssetState{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CloudAssetState{}, err
	}
	defer tx.Rollback()
	deadline, err := r.shareRetentionInTransaction(ctx, tx, owner, now)
	if errors.Is(err, ErrShareNotAvailable) {
		return CloudAssetState{}, ErrNotFound
	}
	if err != nil {
		return CloudAssetState{}, err
	}
	state, err := scanAssetState(tx.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM album_asset_lifecycle WHERE owner_id=? AND asset_id=? AND revision>0`, owner, asset))
	if err != nil {
		return state, err
	}
	if state.State == "purged" || state.State == "purging" {
		return state, ErrNotFound
	}
	if state.TrashExpiresAt != nil {
		until := minTime(deadline, *state.TrashExpiresAt)
		state.RestoreUntil = &until
	}
	return state, tx.Commit()
}

func (r *MySQLUploads) MutateCloudAsset(ctx context.Context, owner, asset, action string, input CloudAssetMutation, now time.Time) (CloudAssetState, error) {
	if !validOwner(owner) || !resourceIDPattern.MatchString(asset) || !validOwner(input.RequestID) || input.ExpectedRevision < 1 || input.ExpectedRevision == math.MaxInt64 || (action != "trash" && action != "restore") || now.IsZero() {
		return CloudAssetState{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CloudAssetState{}, err
	}
	defer tx.Rollback()
	// Replay is scoped to the owner and still requires an accessible account.
	deadline, err := r.shareRetentionInTransaction(ctx, tx, owner, now)
	if errors.Is(err, ErrShareNotAvailable) {
		return CloudAssetState{}, ErrNotFound
	}
	if err != nil {
		return CloudAssetState{}, err
	}
	// Lock acquisition may cross a cutoff; the request timestamp is not a lease.
	if r.billingNow != nil {
		if current := r.billingNow().UTC(); current.After(now) {
			now = current
		}
	}
	if !now.Before(deadline) {
		return CloudAssetState{}, ErrNotFound
	}
	var oldAsset, oldAction string
	var oldRevision int64
	var encoded []byte
	err = tx.QueryRowContext(ctx, `SELECT asset_id,expected_revision,action,result_json FROM album_asset_operations WHERE owner_id=? AND request_id=?`, owner, input.RequestID).Scan(&oldAsset, &oldRevision, &oldAction, &encoded)
	if err == nil {
		if oldAsset != asset || oldRevision != input.ExpectedRevision || oldAction != action {
			return CloudAssetState{}, ErrConflict
		}
		var result CloudAssetState
		if json.Unmarshal(encoded, &result) != nil {
			return result, ErrConflict
		}
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CloudAssetState{}, err
	}
	state, err := scanAssetState(tx.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM album_asset_lifecycle WHERE owner_id=? AND asset_id=? AND revision>0 FOR UPDATE`, owner, asset))
	if err != nil {
		return state, err
	}
	if state.Revision != input.ExpectedRevision || state.epoch == math.MaxInt64 {
		return state, ErrConflict
	}
	authorizationUntil := deadline
	if action == "restore" && state.TrashExpiresAt != nil {
		authorizationUntil = minTime(deadline, *state.TrashExpiresAt)
	}
	var trashed, expires any
	if action == "trash" {
		if state.State != "active" {
			return state, ErrConflict
		}
		expiry := now.UTC().Add(30 * 24 * time.Hour)
		state.State = "trashed"
		timestamp := now.UTC()
		state.TrashedAt = &timestamp
		state.TrashExpiresAt = &expiry
		until := minTime(expiry, deadline)
		state.RestoreUntil = &until
		trashed, expires = timestamp, expiry
	} else {
		if state.State != "trashed" || state.TrashExpiresAt == nil || !now.Before(*state.TrashExpiresAt) {
			return state, ErrConflict
		}
		state.State = "active"
		state.TrashedAt = nil
		state.TrashExpiresAt = nil
		state.RestoreUntil = nil
	}
	state.Revision++
	state.epoch++
	if _, err = tx.ExecContext(ctx, `UPDATE album_asset_lifecycle SET revision=?,lifecycle_epoch=?,state=?,trashed_at=?,trash_expires_at=? WHERE owner_id=? AND asset_id=?`, state.Revision, state.epoch, state.State, trashed, expires, owner, asset); err != nil {
		return state, err
	}
	// Each version gets a tombstone/upsert, so restoration needs no hidden refetch.
	result, err := tx.ExecContext(ctx, `INSERT INTO album_library_changes(owner_id,asset_id,version_id,revision,state,changed_at,trashed_at,trash_expires_at) SELECT owner_id,asset_id,version_id,?,?,?, ?,? FROM album_asset_versions WHERE owner_id=? AND asset_id=? ORDER BY sequence_id`, state.Revision, state.State, now.UTC(), trashed, expires, owner, asset)
	if err != nil {
		return state, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return state, err
	}
	if count == 0 {
		return state, ErrNotFound
	}
	if action == "trash" {
		if _, err = tx.ExecContext(ctx, `UPDATE album_share_links s JOIN album_asset_versions v ON v.owner_id=s.owner_id AND v.version_id=s.version_id SET s.revoked_at=COALESCE(s.revoked_at,?) WHERE v.owner_id=? AND v.asset_id=?`, now.UTC(), owner, asset); err != nil {
			return state, err
		}
	}
	encoded, err = json.Marshal(state)
	if err != nil {
		return state, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO album_asset_operations(owner_id,request_id,asset_id,expected_revision,action,result_json) VALUES(?,?,?,?,?,?)`, owner, input.RequestID, asset, input.ExpectedRevision, action, encoded); err != nil {
		return state, err
	}
	if r.billingNow != nil && !r.billingNow().UTC().Before(authorizationUntil) {
		return CloudAssetState{}, ErrConflict
	}
	return state, tx.Commit()
}

// Workers retain raw AssetVersion access for verification and cleanup; public
// reads and new processing requests must use the current logical visibility.
func (r *MySQLUploads) VisibleAssetVersion(ctx context.Context, owner, version string) (AssetVersion, error) {
	if !validOwner(owner) || !validOwner(version) {
		return AssetVersion{}, ErrInvalidManifest
	}
	v, err := scanAssetVersion(r.db.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM album_asset_versions v WHERE owner_id=? AND version_id=? AND EXISTS(SELECT 1 FROM album_asset_lifecycle a WHERE a.owner_id=v.owner_id AND a.asset_id=v.asset_id AND a.state='active')`, owner, version))
	if err != nil {
		return v, err
	}
	return v, validateVersionOwner(v, owner)
}

// HTTP upload receipts must not advertise a usable backup after logical trash.
func (r *MySQLUploads) ValidateUploadVisibility(ctx context.Context, u Upload) error {
	state, err := scanAssetState(r.db.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM album_asset_lifecycle WHERE owner_id=? AND asset_id=?`, u.OwnerID, u.Manifest.AssetID))
	if err != nil {
		return err
	}
	if state.State != "active" || u.State != OriginalsVerified && state.epoch != u.LifecycleEpoch {
		return ErrNotFound
	}
	return nil
}
func requireActiveUpload(ctx context.Context, tx *sql.Tx, u Upload) error {
	state, err := ensureActiveAsset(ctx, tx, u.OwnerID, u.Manifest.AssetID)
	if err != nil {
		return err
	}
	if u.State != OriginalsVerified && state.epoch != u.LifecycleEpoch {
		return ErrConflict
	}
	return nil
}
