package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/entitlement"
)

type EntitlementRepository struct{ database *sql.DB }

func NewEntitlementRepository(database *sql.DB) *EntitlementRepository {
	return &EntitlementRepository{database: database}
}

func (repository *EntitlementRepository) ApplyNotification(
	ctx context.Context,
	state entitlement.NotificationState,
) (bool, error) {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = transaction.Rollback() }()
	count, err := affectedRows(transaction.ExecContext(ctx, `
        INSERT INTO app_store_notifications
            (notification_uuid, original_transaction_id, environment, expires_at)
        VALUES (?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE notification_uuid = VALUES(notification_uuid)`,
		state.NotificationUUID,
		state.OriginalTransactionID,
		state.Environment,
		state.ExpiresAt,
	))
	if err != nil {
		return false, err
	}
	if count == 0 {
		if err := transaction.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if _, err := transaction.ExecContext(ctx, `
        UPDATE managed_entitlements
        SET environment = ?, expires_at = ?, updated_at = UTC_TIMESTAMP(6)
        WHERE original_transaction_id = ?`,
		state.Environment,
		state.ExpiresAt,
		state.OriginalTransactionID,
	); err != nil {
		return false, err
	}
	if err := insertOfferRedemption(ctx, transaction, state.Environment, state.TransactionID,
		state.OriginalTransactionID, state.OfferIdentifier, state.OfferType, state.SignedAt, state.ExpiresAt); err != nil {
		return false, err
	}
	if err := transaction.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (repository *EntitlementRepository) Upsert(ctx context.Context, record entitlement.Record) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err = transaction.ExecContext(ctx, `
        INSERT INTO managed_entitlements
            (key_id, original_transaction_id, environment, expires_at, updated_at)
        VALUES (?, ?, ?, ?, UTC_TIMESTAMP(6))
        ON DUPLICATE KEY UPDATE
            original_transaction_id = VALUES(original_transaction_id),
            environment = VALUES(environment),
            expires_at = VALUES(expires_at),
            updated_at = UTC_TIMESTAMP(6)`,
		record.KeyID,
		record.TransactionID,
		record.Environment,
		record.ExpiresAt,
	); err != nil {
		return err
	}
	if err := insertOfferRedemption(ctx, transaction, record.Environment, record.OfferTransactionID,
		record.TransactionID, record.OfferIdentifier, record.OfferType, record.OfferSignedAt, record.ExpiresAt); err != nil {
		return err
	}
	return transaction.Commit()
}

func insertOfferRedemption(ctx context.Context, transaction *sql.Tx, environment, transactionID,
	originalTransactionID, offerIdentifier string, offerType int32, signedAt, expiresAt time.Time) error {
	if transactionID == "" || originalTransactionID == "" || offerIdentifier == "" || offerType <= 0 || signedAt.IsZero() {
		return nil
	}
	transactionHash := sha256.Sum256([]byte(transactionID))
	originalHash := sha256.Sum256([]byte(originalTransactionID))
	_, err := transaction.ExecContext(ctx, `
		INSERT INTO app_store_offer_redemptions
			(environment, transaction_hash, original_transaction_hash, offer_identifier, offer_type, redeemed_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE expires_at = GREATEST(expires_at, VALUES(expires_at))`,
		environment, transactionHash[:], originalHash[:], offerIdentifier, offerType, signedAt.UTC(), expiresAt.UTC())
	return err
}

func (repository *EntitlementRepository) Get(
	ctx context.Context,
	keyID string,
) (entitlement.Record, bool, error) {
	var record entitlement.Record
	record.KeyID = keyID
	err := repository.database.QueryRowContext(ctx, `
        SELECT original_transaction_id, environment, expires_at
        FROM managed_entitlements
        WHERE key_id = ?`, keyID).Scan(
		&record.TransactionID,
		&record.Environment,
		&record.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return entitlement.Record{}, false, nil
	}
	if err != nil {
		return entitlement.Record{}, false, err
	}
	return record, true, nil
}

var _ entitlement.Store = (*EntitlementRepository)(nil)
var _ entitlement.NotificationStore = (*EntitlementRepository)(nil)
