package adminportal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

var (
	ErrOperationConflict = errors.New("idempotency key was used for another operation")
	ErrOperationPending  = errors.New("operation is already processing")
)

type OperationResult struct {
	Status int
	Body   json.RawMessage
}

type OperationStore interface {
	Begin(context.Context, string, string, string, string, [32]byte) (*OperationResult, error)
	Complete(context.Context, string, string, string, int, []byte, time.Time) error
}

type MySQLOperationStore struct{ database *sql.DB }

func NewMySQLOperationStore(database *sql.DB) *MySQLOperationStore {
	return &MySQLOperationStore{database: database}
}

func (store *MySQLOperationStore) Begin(ctx context.Context, userID, appID, key, action string, hash [32]byte) (*OperationResult, error) {
	_, err := store.database.ExecContext(ctx, `
		INSERT INTO admin_operations (admin_user_id, app_id, idempotency_key, action, request_hash)
		VALUES (?, ?, ?, ?, ?)`, userID, appID, key, action, hash[:])
	if err == nil {
		return nil, nil
	}
	var mysqlError *mysqlDriver.MySQLError
	if !errors.As(err, &mysqlError) || mysqlError.Number != 1062 {
		return nil, err
	}
	var storedAction, state string
	var storedHash []byte
	var status sql.NullInt64
	var body []byte
	if err := store.database.QueryRowContext(ctx, `
		SELECT action, request_hash, state, response_status, response_json
		FROM admin_operations WHERE admin_user_id = ? AND app_id = ? AND idempotency_key = ?`, userID, appID, key).
		Scan(&storedAction, &storedHash, &state, &status, &body); err != nil {
		return nil, err
	}
	if storedAction != action || len(storedHash) != len(hash) || !equalBytes(storedHash, hash[:]) {
		return nil, ErrOperationConflict
	}
	if state != "completed" || !status.Valid || len(body) == 0 {
		return nil, ErrOperationPending
	}
	return &OperationResult{Status: int(status.Int64), Body: body}, nil
}

func (store *MySQLOperationStore) Complete(ctx context.Context, userID, appID, key string, status int, body []byte, now time.Time) error {
	result, err := store.database.ExecContext(ctx, `
		UPDATE admin_operations
		SET state = 'completed', response_status = ?, response_json = ?, completed_at = ?
		WHERE admin_user_id = ? AND app_id = ? AND idempotency_key = ? AND state = 'processing'`, status, body, now.UTC(), userID, appID, key)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("admin operation was not processing")
	}
	return nil
}

func operationHash(value any) [32]byte {
	encoded, _ := json.Marshal(value)
	return sha256.Sum256(encoded)
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}

var _ OperationStore = (*MySQLOperationStore)(nil)
