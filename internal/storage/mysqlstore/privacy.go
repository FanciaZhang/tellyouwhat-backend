package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/privacy"
)

type PrivacyRepository struct {
	database *sql.DB
	appID    string
}

func NewPrivacyRepository(database *sql.DB, appID string) *PrivacyRepository {
	return &PrivacyRepository{database: database, appID: appID}
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
				(app_id, key_id, device_id, scope, document_version, granted, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
            ON DUPLICATE KEY UPDATE
                device_id = VALUES(device_id), granted = VALUES(granted), recorded_at = VALUES(recorded_at)`,
			repository.appID, record.KeyID, record.DeviceID, record.Scope, record.DocumentVersion, record.Granted, record.RecordedAt,
		); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (repository *PrivacyRepository) HasGrantedConsents(
	ctx context.Context,
	keyID string,
	requirements []privacy.Consent,
) (bool, error) {
	for _, requirement := range requirements {
		var granted bool
		err := repository.database.QueryRowContext(ctx, `
			SELECT granted FROM privacy_consents
			WHERE app_id = ? AND key_id = ? AND scope = ? AND document_version = ?`,
			repository.appID, keyID, requirement.Scope, requirement.DocumentVersion,
		).Scan(&granted)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !granted {
			return false, nil
		}
	}
	return true, nil
}

func (repository *PrivacyRepository) PlanDeletion(ctx context.Context, principal attestation.Principal) (privacy.DeletionPlan, error) {
	keyRows, err := repository.database.QueryContext(ctx, `
        SELECT key_id, device_id, transaction_id
        FROM app_attest_keys
		WHERE app_id = ? AND (key_id = ? OR (? <> '' AND transaction_id = ?))`,
		repository.appID, principal.KeyID, principal.TransactionID, principal.TransactionID)
	if err != nil {
		return privacy.DeletionPlan{}, err
	}
	var plan privacy.DeletionPlan
	var keyIDs []string
	for keyRows.Next() {
		var value attestation.Principal
		value.AppID = repository.appID
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
	arguments := make([]any, 0, len(keyIDs)+1)
	arguments = append(arguments, repository.appID)
	for _, keyID := range keyIDs {
		arguments = append(arguments, keyID)
	}
	rows, err := repository.database.QueryContext(ctx, `
        SELECT object_id FROM media_objects
		WHERE app_id = ? AND owner_key_id IN (`+placeholders(len(keyIDs))+`) AND deleted_at IS NULL`, arguments...)
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
			`DELETE FROM app_store_notifications WHERE app_id = ? AND original_transaction_id = ?`, repository.appID, transactionID,
		); err != nil {
			return err
		}
		originalHash := sha256.Sum256([]byte(transactionID))
		if _, err := transaction.ExecContext(ctx,
			`DELETE FROM app_store_offer_redemptions WHERE app_id = ? AND original_transaction_hash = ?`, repository.appID, originalHash[:],
		); err != nil {
			return err
		}
	}
	if _, err := transaction.ExecContext(ctx, `
        DELETE FROM app_attest_keys
		WHERE app_id = ? AND (key_id = ? OR (? <> '' AND transaction_id = ?))`,
		repository.appID, principal.KeyID, transactionID, transactionID); err != nil {
		return err
	}
	return transaction.Commit()
}

var _ privacy.Repository = (*PrivacyRepository)(nil)
