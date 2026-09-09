package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/entitlement"
)

type EntitlementRepository struct {
	database *sql.DB
	appID    string
}

func NewEntitlementRepository(database *sql.DB, appID string) *EntitlementRepository {
	return &EntitlementRepository{database: database, appID: appID}
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
			(app_id, notification_uuid, original_transaction_id, environment, expires_at)
		VALUES (?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE notification_uuid = VALUES(notification_uuid)`,
		repository.appID,
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
        SET environment = ?, expires_at = ?, price_milli = ?, updated_at = UTC_TIMESTAMP(6)
		WHERE app_id = ? AND original_transaction_id = ? AND environment = ?`,
		state.Environment,
		state.ExpiresAt,
		purchasePrice(state.CurrentPayment),
		repository.appID,
		state.OriginalTransactionID,
		state.Environment,
	); err != nil {
		return false, err
	}
	if err := observePurchase(ctx, transaction, repository.appID, "", state.Environment, state.Payment); err != nil {
		return false, err
	}
	if err := insertOfferRedemption(ctx, transaction, repository.appID, state.Environment, state.TransactionID,
		state.OriginalTransactionID, state.OfferIdentifier, state.ProductID, state.OfferType, state.SignedAt, state.ExpiresAt); err != nil {
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
	if err := repository.upsert(ctx, transaction, record); err != nil {
		return err
	}
	return transaction.Commit()
}

func (repository *EntitlementRepository) UpsertVerified(ctx context.Context, record entitlement.Record) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	var boundID string
	if err := transaction.QueryRowContext(ctx, `SELECT transaction_id FROM app_attest_keys
		WHERE app_id = ? AND key_id = ? FOR UPDATE`, repository.appID, record.KeyID).Scan(&boundID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return entitlement.ErrSubscriptionBindingConflict
		}
		return err
	}
	var existing entitlement.Record
	err = transaction.QueryRowContext(ctx, `SELECT original_transaction_id, environment FROM managed_entitlements
		WHERE app_id = ? AND key_id = ? FOR UPDATE`, repository.appID, record.KeyID).Scan(&existing.TransactionID, &existing.Environment)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !entitlement.CanBindVerifiedTransaction(boundID, existing, record) {
		return entitlement.ErrSubscriptionBindingConflict
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE app_attest_keys SET transaction_id = ?, updated_at = UTC_TIMESTAMP(6)
		WHERE app_id = ? AND key_id = ?`, record.TransactionID, repository.appID, record.KeyID); err != nil {
		return err
	}
	if err := repository.upsert(ctx, transaction, record); err != nil {
		return err
	}
	return transaction.Commit()
}

func (repository *EntitlementRepository) upsert(ctx context.Context, transaction *sql.Tx, record entitlement.Record) error {
	if _, err := transaction.ExecContext(ctx, `
        INSERT INTO managed_entitlements
			(app_id, key_id, original_transaction_id, environment, expires_at, started_at, price_milli, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, UTC_TIMESTAMP(6))
        ON DUPLICATE KEY UPDATE
            original_transaction_id = VALUES(original_transaction_id),
            environment = VALUES(environment),
            expires_at = VALUES(expires_at),
            started_at = COALESCE(VALUES(started_at), started_at),
 price_milli = VALUES(price_milli),
            updated_at = UTC_TIMESTAMP(6)`,
		repository.appID,
		record.KeyID,
		record.TransactionID,
		record.Environment,
		record.ExpiresAt,
		nullableVoiceDate(record.StartedAt),
		purchasePrice(record.Payment),
	); err != nil {
		return err
	}
	if err := observePurchase(ctx, transaction, repository.appID, record.KeyID, record.Environment, record.Payment); err != nil {
		return err
	}
	if err := insertOfferRedemption(ctx, transaction, repository.appID, record.Environment, record.OfferTransactionID,
		record.TransactionID, record.OfferIdentifier, record.ProductID, record.OfferType, record.OfferSignedAt, record.ExpiresAt); err != nil {
		return err
	}
	return nil
}

func insertOfferRedemption(ctx context.Context, transaction *sql.Tx, appID, environment, transactionID,
	originalTransactionID, offerIdentifier, productID string, offerType int32, signedAt, expiresAt time.Time) error {
	if transactionID == "" || originalTransactionID == "" || offerIdentifier == "" || offerType <= 0 || signedAt.IsZero() {
		return nil
	}
	transactionHash := sha256.Sum256([]byte(transactionID))
	originalHash := sha256.Sum256([]byte(originalTransactionID))
	_, err := transaction.ExecContext(ctx, `
		INSERT INTO app_store_offer_redemptions
			(app_id, environment, transaction_hash, original_transaction_hash, offer_identifier, product_id, offer_type, redeemed_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE product_id = IF(product_id='', VALUES(product_id), product_id), expires_at = GREATEST(expires_at, VALUES(expires_at))`,
		appID, environment, transactionHash[:], originalHash[:], offerIdentifier, productID, offerType, signedAt.UTC(), expiresAt.UTC())
	return err
}

func (repository *EntitlementRepository) Get(
	ctx context.Context,
	keyID string,
) (entitlement.Record, bool, error) {
	var record entitlement.Record
	var started sql.NullTime
	record.KeyID = keyID
	err := repository.database.QueryRowContext(ctx, `
        SELECT original_transaction_id, environment, expires_at, started_at
        FROM managed_entitlements
		WHERE app_id = ? AND key_id = ?`, repository.appID, keyID).Scan(
		&record.TransactionID,
		&record.Environment,
		&record.ExpiresAt,
		&started,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return entitlement.Record{}, false, nil
	}
	if err != nil {
		return entitlement.Record{}, false, err
	}
	if started.Valid {
		record.StartedAt = started.Time
	}
	return record, true, nil
}

var _ entitlement.Store = (*EntitlementRepository)(nil)
var _ entitlement.NotificationStore = (*EntitlementRepository)(nil)

func nullableVoiceDate(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}
