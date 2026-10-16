package albums

import (
	"context"
	"database/sql"
	"errors"
)

var ErrRecoveryQuarantined = errors.New("album recovery database is quarantined")

func checkRecoveryState(row rowScanner) error {
	var quarantined bool
	if err := row.Scan(&quarantined); err != nil {
		return err
	}
	if quarantined {
		return ErrRecoveryQuarantined
	}
	return nil
}

// Startup admission precedes Redis and provider construction. This does not
// release a restored database; a recovery cutover workflow is still required.
func CheckRecoveryState(ctx context.Context, db *sql.DB) error {
	return checkRecoveryState(db.QueryRowContext(ctx, `SELECT quarantined FROM album_recovery_state WHERE singleton_id=1`))
}

// A current shared lock prevents an old repeatable-read snapshot from bypassing
// quarantine while allowing independent media transactions to run concurrently.
func lockRecoveryState(ctx context.Context, tx *sql.Tx) error {
	return checkRecoveryState(tx.QueryRowContext(ctx, `SELECT quarantined FROM album_recovery_state WHERE singleton_id=1 FOR SHARE`))
}
