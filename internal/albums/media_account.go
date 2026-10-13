package albums

import (
	"context"
	"database/sql"
	"errors"
)

// Use the media writers' storage lock, never acquire the identity/account lock
// underneath it. A locking read also rejects pre-deletion transaction snapshots.
// Receipt reconciliation and physical cleanup deliberately keep raw storage access.
func requireMediaAccount(ctx context.Context, tx *sql.Tx, owner string) error {
	var deleting sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT deletion_requested_at FROM album_storage_accounts WHERE owner_id=? FOR UPDATE`, owner).Scan(&deleting)
	if errors.Is(err, sql.ErrNoRows) || err == nil && deleting.Valid {
		return ErrOwner
	}
	return err
}
