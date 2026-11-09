package albums

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

type retentionCleanupClaim struct {
	owner, token, cursor string
	until                time.Time
}

type mediaCleanupMode struct{ trash, retention bool }

func (r *MySQLUploads) retentionCleanupCandidates(ctx context.Context, now time.Time, limit int, mode mediaCleanupMode) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT s.owner_id FROM album_storage_accounts s
 JOIN album_accounts a ON a.account_id=s.owner_id AND a.state='active'
 WHERE s.deletion_requested_at IS NULL AND (s.retention_cleanup_after IS NULL OR s.retention_cleanup_after<=?)
 AND (s.retention_cleanup_until IS NULL OR s.retention_cleanup_until<=?) AND (
 (? AND EXISTS(SELECT 1 FROM album_asset_lifecycle l WHERE l.owner_id=s.owner_id AND l.state='trashed' AND l.trash_expires_at<=? AND EXISTS(SELECT 1 FROM album_asset_versions v WHERE v.owner_id=l.owner_id AND v.asset_id=l.asset_id)))
 OR (? AND (
 EXISTS(SELECT 1 FROM album_object_purges p WHERE p.owner_id=s.owner_id AND p.reason='retention' AND p.environment=? AND p.started_at IS NOT NULL AND p.state<>'completed')
 OR (EXISTS(SELECT 1 FROM album_cloud_subscriptions b WHERE b.owner_id=s.owner_id AND b.environment=? GROUP BY b.owner_id HAVING DATE_ADD(MAX(b.expires_at),INTERVAL 6 MONTH)<=?)
 AND (EXISTS(SELECT 1 FROM album_collections c WHERE c.owner_id=s.owner_id AND (c.deleted=FALSE OR EXISTS(SELECT 1 FROM album_collection_members m WHERE m.owner_id=c.owner_id AND m.collection_id=c.collection_id)))
 OR EXISTS(SELECT 1 FROM album_asset_lifecycle l WHERE l.owner_id=s.owner_id AND l.state IN ('active','trashed') AND EXISTS(SELECT 1 FROM album_asset_versions v WHERE v.owner_id=l.owner_id AND v.asset_id=l.asset_id)))))
 )) ORDER BY COALESCE(s.retention_cleanup_after,'1970-01-01'),s.owner_id LIMIT ?`, now, now, mode.trash, now, mode.retention, r.billingEnvironment, r.billingEnvironment, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

func (r *MySQLUploads) claimRetentionCleanup(ctx context.Context, owner string, now func() time.Time) (retentionCleanupClaim, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return retentionCleanupClaim{}, err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, owner); err != nil {
		return retentionCleanupClaim{}, err
	}
	var after, until, deleting sql.NullTime
	var cursor sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT retention_cleanup_after,retention_cleanup_until,retention_cleanup_cursor,deletion_requested_at FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&after, &until, &cursor, &deleting); err != nil {
		return retentionCleanupClaim{}, err
	}
	at := now().UTC().Truncate(time.Microsecond)
	if at.IsZero() || deleting.Valid || after.Valid && after.Time.After(at) || until.Valid && until.Time.After(at) {
		return retentionCleanupClaim{}, ErrLease
	}
	c := retentionCleanupClaim{owner, uuid.NewString(), cursor.String, at.Add(2 * time.Minute)}
	if _, err := tx.ExecContext(ctx, `UPDATE album_storage_accounts SET retention_cleanup_token=?,retention_cleanup_until=? WHERE owner_id=?`, c.token, c.until, owner); err != nil {
		return c, err
	}
	return c, tx.Commit()
}

func (r *MySQLUploads) finishRetentionCleanup(ctx context.Context, c retentionCleanupClaim, cursor string, failed bool, now time.Time) error {
	delay := 5 * time.Second
	if failed {
		delay = time.Minute
	}
	result, err := r.db.ExecContext(ctx, `UPDATE album_storage_accounts SET retention_cleanup_after=?,retention_cleanup_cursor=NULLIF(?,''),retention_cleanup_token=NULL,retention_cleanup_until=NULL
 WHERE owner_id=? AND retention_cleanup_token=? AND retention_cleanup_until=? AND retention_cleanup_until>?`, now.Add(delay), cursor, c.owner, c.token, c.until, now)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err == nil && changed != 1 {
		err = ErrLease
	}
	return err
}

func (r *MySQLUploads) retentionCleanupAssets(ctx context.Context, owner, cursor string, started bool, limit int, now time.Time, mode mediaCleanupMode) ([]string, error) {
	query := `SELECT l.asset_id FROM album_asset_lifecycle l WHERE l.owner_id=? AND l.asset_id>?
 AND ((? AND l.state IN ('active','trashed')) OR (? AND l.state='trashed' AND l.trash_expires_at<=?))
 AND EXISTS(SELECT 1 FROM album_asset_versions v WHERE v.owner_id=l.owner_id AND v.asset_id=l.asset_id)
 AND NOT EXISTS(SELECT 1 FROM album_object_purges p WHERE p.owner_id=l.owner_id AND p.asset_id=l.asset_id AND p.started_at IS NOT NULL)
 ORDER BY l.asset_id LIMIT ?`
	args := []any{owner, cursor, mode.retention, mode.trash, now, limit}
	if started {
		query = `SELECT p.asset_id FROM album_object_purges p WHERE p.owner_id=? AND p.state<>'completed'
 AND ((? AND p.reason='retention' AND p.environment=? AND p.started_at IS NOT NULL)
 OR (? AND p.reason='trash' AND p.trash_expires_at<=?))
 AND p.available_at<=? AND (p.lease_until IS NULL OR p.lease_until<=?) ORDER BY p.available_at,p.asset_id LIMIT ?`
		args = []any{owner, mode.retention, r.billingEnvironment, mode.trash, now, now, now, limit}
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
