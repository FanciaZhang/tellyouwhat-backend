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
	for _, denied := range []entitlement.Record{{KeyID: key.KeyID, TransactionID: "other-paid", Environment: "production", ExpiresAt: production.ExpiresAt}} {
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

func TestMySQLVerifiedSubscriptionEnvironmentRoundTrip(t *testing.T) {
	for _, first := range []string{"sandbox", "production"} {
		t.Run(first, func(t *testing.T) {
			db := testutil.MySQL(t)
			ctx := context.Background()
			keys := mysqlstore.NewKeyRepository(db, "health")
			store := mysqlstore.NewEntitlementRepository(db, "health")
			key := attestation.RegisteredKey{KeyID: "round-trip", DeviceID: uuid.NewString(), PublicKey: []byte("fixture"), Environment: "production", Receipt: []byte("fixture")}
			if err := keys.Register(ctx, key); err != nil {
				t.Fatal(err)
			}
			makeRecord := func(env string) entitlement.Record {
				return entitlement.Record{KeyID: key.KeyID, TransactionID: env + "-purchase", Environment: env, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
			}
			// Seed the pre-migration state: existing verified entitlement and active
			// binding, with no per-environment anchors populated yet.
			legacy := makeRecord(first)
			if err := keys.BindTransaction(ctx, key.KeyID, legacy.TransactionID); err != nil {
				t.Fatal(err)
			}
			if err := store.Upsert(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			other := "production"
			if first == other {
				other = "sandbox"
			}
			assertState := func(want entitlement.Record) {
				t.Helper()
				got, ok, err := store.Get(ctx, key.KeyID)
				bound, keyErr := keys.Get(ctx, key.KeyID)
				if err != nil || keyErr != nil || !ok || got.TransactionID != want.TransactionID || got.Environment != want.Environment || bound.TransactionID != want.TransactionID || !got.ExpiresAt.Equal(want.ExpiresAt) {
					t.Fatalf("inconsistent active subscription: %+v, %v, %v", got, err, keyErr)
				}
			}
			// A failed persistence operation cannot leave the device or anchors
			// switched to the other environment.
			broken := makeRecord(other)
			broken.OfferIdentifier, broken.OfferTransactionID = strings.Repeat("x", 1024), "broken-offer"
			broken.OfferType, broken.OfferSignedAt = 3, time.Now()
			if err := store.UpsertVerified(ctx, broken); err == nil {
				t.Fatal("expected persistence failure")
			}
			assertState(legacy)
			var productionAnchor, sandboxAnchor string
			if err := db.QueryRow(`SELECT production_transaction_id, sandbox_transaction_id FROM app_attest_keys WHERE app_id='health' AND key_id=?`, key.KeyID).Scan(&productionAnchor, &sandboxAnchor); err != nil || productionAnchor != "" || sandboxAnchor != "" {
				t.Fatalf("anchors survived rollback: %v", err)
			}
			var active entitlement.Record
			for _, env := range []string{other, first, other, first} {
				active = makeRecord(env)
				if err := store.UpsertVerified(ctx, active); err != nil {
					t.Fatalf("switch to %s: %v", env, err)
				}
				assertState(active)
				// Neither a different production purchase nor a different sandbox
				// purchase can use an environment hop to replace an existing anchor.
				for _, deniedEnv := range []string{"production", "sandbox"} {
					denied := makeRecord(deniedEnv)
					denied.TransactionID = "other-purchase"
					if err := store.UpsertVerified(ctx, denied); !errors.Is(err, entitlement.ErrSubscriptionBindingConflict) {
						t.Fatalf("replacement accepted: %v", err)
					}
					assertState(active)
				}
			}
			if err := db.QueryRow(`SELECT production_transaction_id, sandbox_transaction_id FROM app_attest_keys WHERE app_id='health' AND key_id=?`, key.KeyID).Scan(&productionAnchor, &sandboxAnchor); err != nil || productionAnchor != "production-purchase" || sandboxAnchor != "sandbox-purchase" {
				t.Fatalf("lost environment binding: %v", err)
			}
			// A notification updates only the matching active environment.
			for _, env := range []string{"sandbox", "production"} {
				id := makeRecord(env).TransactionID
				expiry := active.ExpiresAt
				if env != active.Environment {
					expiry = time.Now().Add(-time.Hour)
				}
				if _, err := store.ApplyNotification(ctx, entitlement.NotificationState{NotificationUUID: uuid.NewString(), OriginalTransactionID: id, Environment: env, ExpiresAt: expiry}); err != nil {
					t.Fatal(err)
				}
				assertState(active)
			}
			privacyStore := mysqlstore.NewPrivacyRepository(db, "health")
			principal := attestation.Principal{KeyID: key.KeyID, DeviceID: key.DeviceID, TransactionID: active.TransactionID}
			plan, err := privacyStore.PlanDeletion(ctx, principal)
			if err != nil || len(plan.Principals) != 2 {
				t.Fatalf("missing environment cache deletion: %+v %v", plan, err)
			}
			if err := privacyStore.DeletePrincipal(ctx, principal); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"app_attest_keys", "managed_entitlements", "app_store_notifications"} {
				var count int
				if err := db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE app_id='health'").Scan(&count); err != nil || count != 0 {
					t.Fatalf("environment state retained in %s: %d %v", table, count, err)
				}
			}
		})
	}
}
