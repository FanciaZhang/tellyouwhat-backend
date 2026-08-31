package mysqlstore

import (
	"context"
	"database/sql"

	"github.com/tellyouwhat/backend/internal/usage"
)

type UsageRepository struct{ database *sql.DB }

func NewUsageRepository(database *sql.DB) *UsageRepository {
	return &UsageRepository{database: database}
}

func (repository *UsageRepository) Record(ctx context.Context, record usage.Record) error {
	_, err := repository.database.ExecContext(ctx, `
        INSERT INTO usage_ledger
            (request_id, key_id, device_id, original_transaction_id, operation,
             input_tokens, output_tokens, occurred_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE request_id = VALUES(request_id)`,
		record.RequestID, record.KeyID, record.DeviceID, record.TransactionID,
		record.Operation, record.InputTokens, record.OutputTokens, record.OccurredAt,
	)
	return err
}

var _ usage.Recorder = (*UsageRepository)(nil)
