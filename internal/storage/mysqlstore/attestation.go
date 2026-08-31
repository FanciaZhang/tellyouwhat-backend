package mysqlstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/tellyouwhat/backend/internal/attestation"
)

type KeyRepository struct{ database *sql.DB }

func NewKeyRepository(database *sql.DB) *KeyRepository {
	return &KeyRepository{database: database}
}

func (repository *KeyRepository) Register(ctx context.Context, key attestation.RegisteredKey) error {
	count, err := affectedRows(repository.database.ExecContext(ctx, `
        INSERT INTO app_attest_keys
            (key_id, device_id, transaction_id, public_key_der, assertion_counter, environment, receipt)
        VALUES (?, ?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE key_id = VALUES(key_id)`,
		key.KeyID,
		key.DeviceID,
		key.TransactionID,
		key.PublicKey,
		key.Counter,
		key.Environment,
		key.Receipt,
	))
	if err != nil {
		return err
	}
	if count != 1 {
		return attestation.ErrKeyAlreadyRegistered
	}
	return nil
}

func (repository *KeyRepository) Get(ctx context.Context, keyID string) (attestation.RegisteredKey, error) {
	var key attestation.RegisteredKey
	var counter int64
	err := repository.database.QueryRowContext(ctx, `
        SELECT key_id, device_id, transaction_id, public_key_der,
               assertion_counter, environment, receipt
        FROM app_attest_keys
        WHERE key_id = ?`, keyID).Scan(
		&key.KeyID,
		&key.DeviceID,
		&key.TransactionID,
		&key.PublicKey,
		&counter,
		&key.Environment,
		&key.Receipt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return attestation.RegisteredKey{}, attestation.ErrKeyNotFound
	}
	if err != nil {
		return attestation.RegisteredKey{}, err
	}
	key.Counter = uint32(counter)
	return key, nil
}

func (repository *KeyRepository) AdvanceCounter(
	ctx context.Context,
	keyID string,
	expected,
	next uint32,
) error {
	count, err := affectedRows(repository.database.ExecContext(ctx, `
        UPDATE app_attest_keys
        SET assertion_counter = ?, updated_at = UTC_TIMESTAMP(6)
        WHERE key_id = ? AND assertion_counter = ? AND ? > ?`,
		next, keyID, expected, next, expected,
	))
	if err != nil {
		return err
	}
	if count != 1 {
		return attestation.ErrReplay
	}
	return nil
}

func (repository *KeyRepository) BindTransaction(
	ctx context.Context,
	keyID string,
	transactionID string,
) error {
	result, err := repository.database.ExecContext(ctx, `
        UPDATE app_attest_keys
        SET transaction_id = ?, updated_at = UTC_TIMESTAMP(6)
        WHERE key_id = ? AND (transaction_id = '' OR transaction_id = ?)`,
		transactionID, keyID, transactionID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 1 {
		return nil
	}
	var existing string
	if err := repository.database.QueryRowContext(ctx,
		`SELECT transaction_id FROM app_attest_keys WHERE key_id = ?`, keyID,
	).Scan(&existing); err != nil || existing != transactionID {
		return attestation.ErrTransactionBindingConflict
	}
	return nil
}

var _ attestation.KeyStore = (*KeyRepository)(nil)
var _ attestation.EnrollmentKeyStore = (*KeyRepository)(nil)
