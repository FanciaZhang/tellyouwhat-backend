package albums

import (
	"context"
	"database/sql"
	"errors"
)

// Use the media writers' storage lock, never acquire the identity/account lock
// underneath it. A locking read also rejects pre-deletion transaction snapshots.
// Reconciliation and cleanup bypass the account deletion fence, not recovery quarantine.
func requireMediaAccount(ctx context.Context, tx *sql.Tx, owner string) error {
	if err := lockRecoveryState(ctx, tx); err != nil {
		return err
	}
	var deleting sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT deletion_requested_at FROM album_storage_accounts WHERE owner_id=? FOR UPDATE`, owner).Scan(&deleting)
	if errors.Is(err, sql.ErrNoRows) || err == nil && deleting.Valid {
		return ErrOwner
	}
	return err
}
