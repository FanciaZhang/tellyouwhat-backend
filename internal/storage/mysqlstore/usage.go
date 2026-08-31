package mysqlstore

import (
	"context"
	"database/sql"

	"github.com/tellyouwhat/backend/internal/usage"
)

type UsageRepository struct {
	database *sql.DB
	appID    string
}

func NewUsageRepository(database *sql.DB, appID string) *UsageRepository {
	return &UsageRepository{database: database, appID: appID}
}

func (repository *UsageRepository) Record(ctx context.Context, record usage.Record) error {
	_, err := repository.database.ExecContext(ctx, `
        INSERT INTO usage_ledger
			(app_id, request_id, key_id, device_id, original_transaction_id, operation,
             input_tokens, output_tokens, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE request_id = VALUES(request_id)`,
		repository.appID, record.RequestID, record.KeyID, record.DeviceID, record.TransactionID,
		record.Operation, record.InputTokens, record.OutputTokens, record.OccurredAt,
	)
	return err
}

var _ usage.Recorder = (*UsageRepository)(nil)
