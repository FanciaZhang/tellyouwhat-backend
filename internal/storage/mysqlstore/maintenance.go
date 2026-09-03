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

type ExpiredMediaDeleter interface {
	DeleteObject(context.Context, string) error
}

func NewMaintenanceRepository(database *sql.DB) *MaintenanceRepository {
	return &MaintenanceRepository{database: database}
}

// PurgeExpiredMedia removes the object before forgetting its storage key.
// A failed deletion keeps the row available for the next maintenance run.
func (repository *MaintenanceRepository) PurgeExpiredMedia(ctx context.Context, now time.Time, deleter ExpiredMediaDeleter) (int, error) {
	if repository == nil || repository.database == nil || now.IsZero() || deleter == nil {
		return 0, errors.New("invalid expired media cleanup")
	}
	count := 0
	for {
		rows, err := repository.database.QueryContext(ctx,
			`SELECT app_id, object_id FROM media_objects WHERE expires_at <= ? ORDER BY expires_at LIMIT 100`, now)
		if err != nil {
			return count, err
		}
		var expired []struct{ appID, objectID string }
		for rows.Next() {
			var value struct{ appID, objectID string }
			if err := rows.Scan(&value.appID, &value.objectID); err != nil {
				_ = rows.Close()
				return count, err
			}
			expired = append(expired, value)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return count, err
		}
		if len(expired) == 0 {
			return count, nil
		}
		for _, value := range expired {
			if err := deleter.DeleteObject(ctx, value.objectID); err != nil {
				return count, errors.New("expired media object deletion failed; metadata retained for retry")
			}
			removed, err := affectedRows(repository.database.ExecContext(ctx,
				`DELETE FROM media_objects WHERE app_id = ? AND object_id = ? AND expires_at <= ?`, value.appID, value.objectID, now))
			if err != nil {
				return count, err
			}
			count += int(removed)
		}
	}
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
		{`DELETE FROM media_objects WHERE expires_at <= ? AND deleted_at IS NOT NULL`, []any{now}},
		{`DELETE FROM ai_jobs WHERE status IN ('succeeded', 'failed', 'cancelled') AND expires_at <= ?`, []any{now}},
		{`DELETE FROM app_store_notifications WHERE processed_at <= ?`, []any{auditCutoff}},
		{`DELETE FROM app_store_offer_redemptions WHERE redeemed_at <= ?`, []any{auditCutoff}},
		{`DELETE FROM admin_operations WHERE created_at <= ?`, []any{auditCutoff}},
		{`DELETE FROM admin_audit_events WHERE created_at <= ?`, []any{auditCutoff}},
		{`DELETE FROM usage_ledger WHERE occurred_at <= ?`, []any{auditCutoff}},
		{`DELETE app_attest_keys FROM app_attest_keys
           LEFT JOIN managed_entitlements ON managed_entitlements.app_id = app_attest_keys.app_id
                                         AND managed_entitlements.key_id = app_attest_keys.key_id
           WHERE app_attest_keys.updated_at <= ?
             AND (managed_entitlements.key_id IS NULL
                  OR (managed_entitlements.expires_at IS NOT NULL AND managed_entitlements.expires_at <= ?))
             AND NOT EXISTS (
                 SELECT 1 FROM media_objects
                 WHERE media_objects.app_id = app_attest_keys.app_id
                   AND media_objects.owner_key_id = app_attest_keys.key_id
             )`, []any{identityCutoff, identityCutoff}},
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
