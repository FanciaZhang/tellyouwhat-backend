package mysqlstore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

func TestMySQLDeveloperAccessKeepsDevicesSeparateWithoutCreatingPurchases(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	keys := mysqlstore.NewKeyRepository(db, "health")
	store := mysqlstore.NewEntitlementRepository(db, "health")
	service := entitlement.NewDevelopmentService("health", store, 30*24*time.Hour, func() time.Time { return now })
	for index := range 2 {
		key := attestation.RegisteredKey{KeyID: fmt.Sprintf("development-%d", index), DeviceID: uuid.NewString(),
			PublicKey: []byte("fixture"), Environment: "production", Receipt: []byte("fixture")}
		if err := keys.Register(ctx, key); err != nil {
			t.Fatal(err)
		}
		principal := attestation.Principal{AppID: "health", KeyID: key.KeyID, DeviceID: key.DeviceID}
		expires, err := service.Activate(ctx, principal, "app-attest")
		if err != nil || !expires.Equal(now.Add(30*24*time.Hour)) {
			t.Fatalf("device activation failed: %v", err)
		}
		record, found, err := store.Get(ctx, key.KeyID)
		if err != nil || !found || record.Environment != "development" || record.TransactionID != "" || record.Payment.HasPrice() {
			t.Fatal("developer access was persisted as a purchase")
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM managed_entitlements WHERE environment='development' AND original_transaction_id='' AND price_milli IS NULL`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("independent developer records incorrect: %d %v", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM app_store_offer_redemptions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("developer access created purchase redemption records")
	}
	paid := entitlement.Record{KeyID: "development-0", TransactionID: "verified-transaction", Environment: "production", ExpiresAt: now.Add(time.Hour)}
	if err := store.Upsert(ctx, paid); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, entitlement.Record{KeyID: paid.KeyID, Environment: "development", ExpiresAt: now.Add(30 * 24 * time.Hour)}); !errors.Is(err, entitlement.ErrActivationDenied) {
		t.Fatal("stale development write bypassed the purchase promotion lock")
	}
	if _, err := service.Activate(ctx, attestation.Principal{AppID: "health", KeyID: paid.KeyID, DeviceID: "device"}, "app-attest"); !errors.Is(err, entitlement.ErrActivationDenied) {
		t.Fatal("developer activation overwrote a verified purchase")
	}
	retained, _, _ := store.Get(ctx, paid.KeyID)
	if retained.Environment != "production" || retained.TransactionID != paid.TransactionID || !retained.ExpiresAt.Equal(paid.ExpiresAt) {
		t.Fatal("verified purchase changed")
	}
}
