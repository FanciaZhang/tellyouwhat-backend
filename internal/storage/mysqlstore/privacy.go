package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/privacy"
)

type PrivacyRepository struct{ database *sql.DB }

func NewPrivacyRepository(database *sql.DB) *PrivacyRepository {
	return &PrivacyRepository{database: database}
}

func (repository *PrivacyRepository) RecordConsents(ctx context.Context, records []privacy.Record) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	for _, record := range records {
		if _, err := transaction.ExecContext(ctx, `
            INSERT INTO privacy_consents
                (key_id, device_id, scope, document_version, granted, recorded_at)
            VALUES (?, ?, ?, ?, ?, ?)
            ON DUPLICATE KEY UPDATE
                device_id = VALUES(device_id), granted = VALUES(granted), recorded_at = VALUES(recorded_at)`,
			record.KeyID, record.DeviceID, record.Scope, record.DocumentVersion, record.Granted, record.RecordedAt,
		); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (repository *PrivacyRepository) PlanDeletion(ctx context.Context, principal attestation.Principal) (privacy.DeletionPlan, error) {
	keyRows, err := repository.database.QueryContext(ctx, `
        SELECT key_id, device_id, transaction_id
        FROM app_attest_keys
        WHERE key_id = ? OR (? <> '' AND transaction_id = ?)`,
		principal.KeyID, principal.TransactionID, principal.TransactionID)
	if err != nil {
		return privacy.DeletionPlan{}, err
	}
	var plan privacy.DeletionPlan
	var keyIDs []string
	for keyRows.Next() {
		var value attestation.Principal
		if err := keyRows.Scan(&value.KeyID, &value.DeviceID, &value.TransactionID); err != nil {
			keyRows.Close()
			return privacy.DeletionPlan{}, err
		}
		plan.Principals = append(plan.Principals, value)
		keyIDs = append(keyIDs, value.KeyID)
	}
	if err := keyRows.Close(); err != nil {
		return privacy.DeletionPlan{}, err
	}
	if err := keyRows.Err(); err != nil {
		return privacy.DeletionPlan{}, err
	}
	if len(keyIDs) == 0 {
		return privacy.DeletionPlan{Principals: []attestation.Principal{principal}}, nil
	}
	arguments := make([]any, len(keyIDs))
	for index, keyID := range keyIDs {
		arguments[index] = keyID
	}
	rows, err := repository.database.QueryContext(ctx, `
        SELECT object_id FROM media_objects
        WHERE owner_key_id IN (`+placeholders(len(keyIDs))+`) AND deleted_at IS NULL`, arguments...)
	if err != nil {
		return privacy.DeletionPlan{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var objectID string
		if err := rows.Scan(&objectID); err != nil {
			return privacy.DeletionPlan{}, err
		}
		plan.MediaObjectIDs = append(plan.MediaObjectIDs, objectID)
	}
	return plan, rows.Err()
}

func (repository *PrivacyRepository) DeletePrincipal(ctx context.Context, principal attestation.Principal) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	transactionID := principal.TransactionID
	if transactionID != "" {
		if _, err := transaction.ExecContext(ctx,
			`DELETE FROM app_store_notifications WHERE original_transaction_id = ?`, transactionID,
		); err != nil {
			return err
		}
		originalHash := sha256.Sum256([]byte(transactionID))
		if _, err := transaction.ExecContext(ctx,
			`DELETE FROM app_store_offer_redemptions WHERE original_transaction_hash = ?`, originalHash[:],
		); err != nil {
			return err
		}
	}
	if _, err := transaction.ExecContext(ctx, `
        DELETE FROM app_attest_keys
        WHERE key_id = ? OR (? <> '' AND transaction_id = ?)`,
		principal.KeyID, transactionID, transactionID); err != nil {
		return err
	}
	return transaction.Commit()
}

var _ privacy.Repository = (*PrivacyRepository)(nil)
