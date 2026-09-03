package mysqlstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

type maintenanceMediaDeleter struct {
	fail bool
	keys []string
}

func (deleter *maintenanceMediaDeleter) DeleteObject(_ context.Context, key string) error {
	if deleter.fail {
		return errors.New("temporary storage outage")
	}
	deleter.keys = append(deleter.keys, key)
	return nil
}

func testMaintenance(t *testing.T, ctx context.Context, database *sql.DB, now time.Time, owner attestation.RegisteredKey) {
	t.Helper()
	repository := mysqlstore.NewMaintenanceRepository(database)
	mediaRepository := mysqlstore.NewMediaRepository(database, "health")
	for index, expiry := range []time.Time{now.Add(-time.Hour), now.Add(-time.Minute), now.Add(time.Hour)} {
		record := media.Record{
			ObjectID: fmt.Sprintf("ai-temp/health/maintenance/%d", index), OwnerKeyID: owner.KeyID,
			OwnerDeviceID: owner.DeviceID, RequestID: "00000000-0000-4000-8000-000000000020",
			Operation: contracts.OperationMealPhotoCapture, MediaID: fmt.Sprint(index), Kind: "image", MIMEType: "image/png",
			SHA256: strings.Repeat("a", 64), SizeBytes: 128, ExpiresAt: expiry,
		}
		if err := mediaRepository.Register(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	deleter := &maintenanceMediaDeleter{fail: true}
	if count, err := repository.PurgeExpiredMedia(ctx, now, deleter); err == nil || count != 0 {
		t.Fatalf("failed object deletion must remain retryable: count=%d err=%v", count, err)
	}
	if _, err := mediaRepository.Get(ctx, "ai-temp/health/maintenance/0"); err != nil {
		t.Fatalf("failed cleanup forgot the object: %v", err)
	}
	deleter.fail = false
	if count, err := repository.PurgeExpiredMedia(ctx, now, deleter); err != nil || count != 2 || len(deleter.keys) != 2 {
		t.Fatalf("expired media retry: count=%d deleted=%d err=%v", count, len(deleter.keys), err)
	}
	if _, err := mediaRepository.Get(ctx, "ai-temp/health/maintenance/2"); err != nil {
		t.Fatalf("cleanup removed an unexpired upload: %v", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO apps (app_id, display_name, bundle_id, managed_product_id)
        VALUES ('journal', 'Journal', 'cn.tellyouwhat.journalapp', 'journal.ai.subscription.monthly')`); err != nil {
		t.Fatal(err)
	}
	for _, appID := range []string{"health", "journal"} {
		key := attestation.RegisteredKey{
			AppID: appID, KeyID: "maintenance-shared-key", DeviceID: "00000000-0000-4000-8000-000000000021",
			PublicKey: []byte("test-public-key"), Receipt: []byte("test-receipt"), Environment: "production",
		}
		if err := mysqlstore.NewKeyRepository(database, appID).Register(ctx, key); err != nil {
			t.Fatal(err)
		}
		if _, err := database.ExecContext(ctx, `UPDATE app_attest_keys SET updated_at = ? WHERE app_id = ? AND key_id = ?`,
			now.AddDate(0, 0, -60), appID, key.KeyID); err != nil {
			t.Fatal(err)
		}
		expiry := now.Add(time.Hour)
		if appID == "health" {
			expiry = now.AddDate(0, 0, -60)
		}
		if err := mysqlstore.NewEntitlementRepository(database, appID).Upsert(ctx, entitlement.Record{
			KeyID: key.KeyID, TransactionID: "maintenance-transaction", Environment: "production", ExpiresAt: expiry,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.Cleanup(ctx, now); err != nil {
		t.Fatal(err)
	}
	var survivingApp string
	if err := database.QueryRowContext(ctx, `SELECT app_id FROM app_attest_keys WHERE key_id = 'maintenance-shared-key'`).Scan(&survivingApp); err != nil || survivingApp != "journal" {
		t.Fatalf("cleanup crossed application boundaries: surviving=%q err=%v", survivingApp, err)
	}
	// An expired subscription does not make a recently active free user inactive.
	recent := attestation.RegisteredKey{
		AppID: "health", KeyID: "maintenance-recent-free", DeviceID: "00000000-0000-4000-8000-000000000022",
		PublicKey: []byte("test-public-key"), Receipt: []byte("test-receipt"), Environment: "production",
	}
	if err := mysqlstore.NewKeyRepository(database, "health").Register(ctx, recent); err != nil {
		t.Fatal(err)
	}
	if err := mysqlstore.NewEntitlementRepository(database, "health").Upsert(ctx, entitlement.Record{
		KeyID: recent.KeyID, TransactionID: "maintenance-recent-transaction", Environment: "production", ExpiresAt: now.AddDate(0, 0, -60),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE managed_entitlements SET expires_at = ? WHERE app_id = ? AND key_id = ?`,
		now.AddDate(0, 0, -60), "health", owner.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Cleanup(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := mysqlstore.NewKeyRepository(database, "health").Get(ctx, recent.KeyID); err != nil {
		t.Fatalf("cleanup removed a recently active free user: %v", err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE app_attest_keys SET updated_at = ? WHERE app_id = ? AND key_id = ?`,
		now.AddDate(0, 0, -60), "health", owner.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Cleanup(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := mediaRepository.Get(ctx, "ai-temp/health/maintenance/2"); err != nil {
		t.Fatalf("identity cleanup forgot an object that still needs storage deletion: %v", err)
	}
}
