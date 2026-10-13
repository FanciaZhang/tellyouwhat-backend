package albums

import (
	"context"
	"database/sql"
	"time"
)

func lockAccountMediaSettlement(ctx context.Context, tx *sql.Tx, owner, request string, at time.Time) (accountObjectPurge, sql.NullTime, error) {
	var p accountObjectPurge
	var settled sql.NullTime
	if err := lockAccountPurgeScope(ctx, tx, owner, request, at); err != nil {
		return p, settled, err
	}
	p, err := scanAccountPurge(tx.QueryRowContext(ctx, `SELECT `+accountPurgeColumns+` FROM album_account_object_purges WHERE account_id=? FOR UPDATE`, owner))
	if err != nil {
		return p, settled, err
	}
	if p.Request != request || p.State != "objects_removed" || at.Before(p.RemovedAt) {
		return p, settled, ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT media_settled_at FROM album_account_object_purges WHERE account_id=? FOR UPDATE`, owner).Scan(&settled); err != nil {
		return p, settled, err
	}
	if !settled.Valid {
		err = requireAccountMediaQuiescence(ctx, tx, owner, request)
	}
	return p, settled, err
}

// Called only after account object removal. No scheduler/HTTP entrypoint is
// enabled yet. Keeps identity, Apple revocation and billing records for their
// separate erasure stages; this receipt never means complete account deletion.
func (r *MySQLUploads) settleAccountMediaPurge(ctx context.Context, owner, request string, storage accountPurgeStorage, now func() time.Time) (time.Time, error) {
	if storage == nil || now == nil || now().IsZero() {
		return time.Time{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	initial, settled, err := lockAccountMediaSettlement(ctx, tx, owner, request, now().UTC())
	if err != nil {
		tx.Rollback()
		return time.Time{}, err
	}
	if err = tx.Commit(); err != nil {
		return time.Time{}, err
	}
	if settled.Valid {
		return settled.Time, nil
	}

	// No database lock is held during provider I/O. A renewed nonempty inventory
	// reopens that exact area without touching media records or releasing quota.
	nonempty := -1
	for i, area := range accountPurgeAreas {
		empty, e := accountPrefixEmpty(ctx, storage, owner, area)
		if e != nil {
			return time.Time{}, e
		}
		if !empty {
			nonempty = i
			break
		}
	}
	tx, err = r.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	at := now().UTC().Truncate(time.Microsecond)
	p, settled, err := lockAccountMediaSettlement(ctx, tx, owner, request, at)
	if err != nil {
		return time.Time{}, err
	}
	if settled.Valid {
		return settled.Time, tx.Commit()
	}
	if !p.RemovedAt.Equal(initial.RemovedAt) {
		return time.Time{}, ErrConflict
	}
	if nonempty >= 0 {
		_, err = tx.ExecContext(ctx, `UPDATE album_account_object_purges SET state='queued',area_index=?,objects_removed_at=NULL,updated_at=? WHERE account_id=?`, nonempty, at, owner)
		if err != nil {
			return time.Time{}, err
		}
		if err = tx.Commit(); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, ErrDerivedPending
	}

	// Children first, always account-scoped. Unpublished uploads and records
	// absent from the catalogue are deliberately included.
	for _, query := range []string{
		`DELETE c FROM album_share_claims c JOIN album_share_links s ON s.share_id=c.share_id WHERE s.owner_id=?`,
		`DELETE FROM album_share_links WHERE owner_id=?`,
		`DELETE FROM album_object_purges WHERE owner_id=?`,
		`DELETE FROM album_asset_operations WHERE owner_id=?`,
		`DELETE FROM album_library_changes WHERE owner_id=?`,
		`DELETE m FROM album_live_pair_multiparts m JOIN album_live_pairs p ON p.pair_upload_id=m.pair_upload_id WHERE p.owner_id=?`,
		`DELETE FROM album_live_pairs WHERE owner_id=?`,
		`DELETE FROM album_derived_cleanups WHERE owner_id=?`,
		`DELETE FROM album_derived_finalizations WHERE owner_id=?`,
		`DELETE FROM album_photo_jobs WHERE owner_id=?`,
		`DELETE FROM album_photo_writes WHERE owner_id=?`,
		`DELETE FROM album_deletion_video_jobs WHERE account_id=?`,
		`DELETE FROM album_video_jobs WHERE owner_id=?`,
		`DELETE c FROM album_multipart_completions c JOIN album_uploads u ON u.upload_id=c.upload_id WHERE u.owner_id=?`,
		`DELETE c FROM album_multipart_intents c JOIN album_uploads u ON u.upload_id=c.upload_id WHERE u.owner_id=?`,
		`DELETE c FROM album_original_seals c JOIN album_uploads u ON u.upload_id=c.upload_id WHERE u.owner_id=?`,
		`DELETE FROM album_uploads WHERE owner_id=?`,
		`DELETE FROM album_asset_versions WHERE owner_id=?`,
		`DELETE FROM album_asset_lifecycle WHERE owner_id=?`,
	} {
		if _, err = tx.ExecContext(ctx, query, owner); err != nil {
			return time.Time{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=0,reserved_bytes=0 WHERE owner_id=?`, owner); err != nil {
		return time.Time{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_account_object_purges SET media_settled_at=?,updated_at=? WHERE account_id=?`, at, at, owner); err != nil {
		return time.Time{}, err
	}
	if err = tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return at, nil
}
