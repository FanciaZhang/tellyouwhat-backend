package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	auditRetentionDays   = 400
	inactiveIdentityDays = 30
)

type CleanupResult struct {
	IdempotencyRecords int
	MediaObjects       int
	Jobs               int
	Notifications      int
	OfferRedemptions   int
	AdminAuditRecords  int
	UsageRecords       int
	IdentityRecords    int
}

type MaintenanceRepository struct {
	database *sql.DB
}

func NewMaintenanceRepository(database *sql.DB) *MaintenanceRepository {
	return &MaintenanceRepository{database: database}
}

func (repository *MaintenanceRepository) Cleanup(ctx context.Context, now time.Time) (CleanupResult, error) {
	if repository == nil || repository.database == nil || now.IsZero() {
		return CleanupResult{}, errors.New("invalid maintenance repository")
	}
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return CleanupResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	auditCutoff := now.AddDate(0, 0, -auditRetentionDays)
	identityCutoff := now.AddDate(0, 0, -inactiveIdentityDays)
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM idempotency_records WHERE expires_at <= ?`, []any{now}},
		{`DELETE FROM media_objects WHERE expires_at <= ?`, []any{now}},
		{`DELETE FROM ai_jobs WHERE status IN ('succeeded', 'failed', 'cancelled') AND expires_at <= ?`, []any{now}},
		{`DELETE FROM app_store_notifications WHERE processed_at <= ?`, []any{auditCutoff}},
		{`DELETE FROM app_store_offer_redemptions WHERE redeemed_at <= ?`, []any{auditCutoff}},
		{`DELETE FROM admin_operations WHERE created_at <= ?`, []any{auditCutoff}},
		{`DELETE FROM admin_audit_events WHERE created_at <= ?`, []any{auditCutoff}},
		{`DELETE FROM usage_ledger WHERE occurred_at <= ?`, []any{auditCutoff}},
		{`DELETE app_attest_keys FROM app_attest_keys
           LEFT JOIN managed_entitlements ON managed_entitlements.key_id = app_attest_keys.key_id
           WHERE (managed_entitlements.expires_at IS NOT NULL AND managed_entitlements.expires_at <= ?)
              OR (managed_entitlements.key_id IS NULL AND app_attest_keys.updated_at <= ?)`, []any{identityCutoff, identityCutoff}},
	}
	counts := make([]int, len(statements))
	for index, statement := range statements {
		count, executeErr := affectedRows(transaction.ExecContext(ctx, statement.query, statement.args...))
		if executeErr != nil {
			return CleanupResult{}, executeErr
		}
		counts[index] = int(count)
	}
	if err := transaction.Commit(); err != nil {
		return CleanupResult{}, err
	}
	return CleanupResult{
		IdempotencyRecords: counts[0],
		MediaObjects:       counts[1],
		Jobs:               counts[2],
		Notifications:      counts[3],
		OfferRedemptions:   counts[4],
		AdminAuditRecords:  counts[5] + counts[6],
		UsageRecords:       counts[7],
		IdentityRecords:    counts[8],
	}, nil
}
