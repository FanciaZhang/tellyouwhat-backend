package mysqlstore_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

func TestMySQLVerifiedEntitlementPromotionIsAtomic(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	keys := mysqlstore.NewKeyRepository(db, "health")
	store := mysqlstore.NewEntitlementRepository(db, "health")
	key := attestation.RegisteredKey{KeyID: "promotion", DeviceID: uuid.NewString(), PublicKey: []byte("fixture"), Environment: "production", Receipt: []byte("fixture")}
	if err := keys.Register(ctx, key); err != nil {
		t.Fatal(err)
	}
	sandbox := entitlement.Record{KeyID: key.KeyID, TransactionID: "sandbox", Environment: "sandbox", ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
	if err := store.UpsertVerified(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	production := sandbox
	production.TransactionID = "paid"
	production.Environment = "production"
	production.ExpiresAt = sandbox.ExpiresAt.Add(30 * 24 * time.Hour)
	production.ProductID = "health.premium.subscription.monthly"
	production.OfferTransactionID = "verified-offer-transaction"
	production.OfferIdentifier = "FRIENDS"
	production.OfferType = 3
	production.OfferSignedAt = time.Now()
	broken := production
	broken.OfferTransactionID = "offer"
	broken.OfferIdentifier = strings.Repeat("x", 1024)
	broken.OfferType = 3
	broken.OfferSignedAt = time.Now()
	if err := store.UpsertVerified(ctx, broken); err == nil {
		t.Fatal("expected a persistence error")
	}
	assertRecord := func(want entitlement.Record) {
		t.Helper()
		k, err := keys.Get(ctx, key.KeyID)
		if err != nil || k.TransactionID != want.TransactionID {
			t.Fatalf("binding not atomic: %v", err)
		}
		got, ok, err := store.Get(ctx, key.KeyID)
		if err != nil || !ok || got.TransactionID != want.TransactionID || got.Environment != want.Environment || !got.ExpiresAt.Equal(want.ExpiresAt) {
			t.Fatalf("entitlement not atomic: %+v %v", got, err)
		}
	}
	assertRecord(sandbox)
	for i := 0; i < 2; i++ {
		if err := store.UpsertVerified(ctx, production); err != nil {
			t.Fatal(err)
		}
	}
	assertRecord(production)
	var product string
	if err := db.QueryRow(`SELECT product_id FROM app_store_offer_redemptions WHERE app_id='health' AND offer_identifier='FRIENDS'`).Scan(&product); err != nil || product != production.ProductID {
		t.Fatalf("verified offer lost product: %q %v", product, err)
	}
	for _, denied := range []entitlement.Record{sandbox, {KeyID: key.KeyID, TransactionID: "other-paid", Environment: "production", ExpiresAt: production.ExpiresAt}} {
		if err := store.UpsertVerified(ctx, denied); !errors.Is(err, entitlement.ErrSubscriptionBindingConflict) {
			t.Fatalf("expected conflict: %v", err)
		}
		assertRecord(production)
	}
	// A late notification for the old test transaction cannot alter paid access.
	if _, err := store.ApplyNotification(ctx, entitlement.NotificationState{NotificationUUID: uuid.NewString(), OriginalTransactionID: sandbox.TransactionID, Environment: "sandbox", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	assertRecord(production)
	// Environment is part of purchase identity even when transaction IDs match.
	if _, err := store.ApplyNotification(ctx, entitlement.NotificationState{NotificationUUID: uuid.NewString(), OriginalTransactionID: production.TransactionID, Environment: "sandbox", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	assertRecord(production)
}

func TestMySQLConcurrentVerifiedPromotionsChooseOnePurchase(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	keys := mysqlstore.NewKeyRepository(db, "health")
	store := mysqlstore.NewEntitlementRepository(db, "health")
	key := attestation.RegisteredKey{KeyID: "concurrent-promotion", DeviceID: uuid.NewString(), PublicKey: []byte("fixture"), Environment: "production", Receipt: []byte("fixture")}
	if err := keys.Register(ctx, key); err != nil {
		t.Fatal(err)
	}
	base := entitlement.Record{KeyID: key.KeyID, TransactionID: "test", Environment: "sandbox", ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.UpsertVerified(ctx, base); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"paid-a", "paid-b"} {
		group.Add(1)
		go func(id string) {
			defer group.Done()
			r := base
			r.TransactionID = id
			r.Environment = "production"
			results <- store.UpsertVerified(ctx, r)
		}(id)
	}
	group.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, entitlement.ErrSubscriptionBindingConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
	k, err := keys.Get(ctx, key.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	r, ok, err := store.Get(ctx, key.KeyID)
	if err != nil || !ok || r.TransactionID != k.TransactionID || r.Environment != "production" {
		t.Fatalf("inconsistent winner: %v", err)
	}
}
