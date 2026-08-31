package entitlement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
)

func TestDevelopmentActivationRequiresRotatableSecretAndExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	service := NewDevelopmentService(store, "expected-secret", 30*24*time.Hour, func() time.Time { return now })
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}

	if _, err := service.Activate(context.Background(), principal, "wrong"); !errors.Is(err, ErrActivationDenied) {
		t.Fatalf("expected denied activation, got %v", err)
	}
	expiresAt, err := service.Activate(context.Background(), principal, "expected-secret")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !expiresAt.Equal(now.Add(30 * 24 * time.Hour)) {
		t.Fatalf("unexpected expiry: %s", expiresAt)
	}
	allowed, _ := service.HasManagedSubscription(context.Background(), principal)
	if !allowed {
		t.Fatal("activated principal should be entitled")
	}
	now = expiresAt.Add(time.Second)
	allowed, _ = service.HasManagedSubscription(context.Background(), principal)
	if allowed {
		t.Fatal("expired development entitlement must be rejected")
	}
}

func TestProductionSyncBindsVerifiedActiveSubscriptionToAttestedKey(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	expiresAt := now.Add(30 * 24 * time.Hour)
	store := NewMemoryStore()
	resolver := &fakeSubscriptionResolver{state: SubscriptionState{
		OriginalTransactionID: "original-transaction-1",
		TransactionID:         "offer-transaction-1",
		Environment:           "Production",
		OfferIdentifier:       "friends-september",
		OfferType:             3,
		ExpiresAt:             expiresAt,
		SignedAt:              now,
	}}
	service := NewProductionService(store, resolver, func() time.Time { return now })
	binder := &fakeTransactionBinder{}
	service.WithTransactionBinder(binder)
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}

	result, err := service.Sync(context.Background(), principal, "signed-transaction")
	if err != nil {
		t.Fatalf("sync production subscription: %v", err)
	}
	if !result.Equal(expiresAt) {
		t.Fatalf("unexpected expiry: %s", result)
	}
	record, ok, err := store.Get(context.Background(), principal.KeyID)
	if err != nil || !ok {
		t.Fatalf("load entitlement: ok=%v err=%v", ok, err)
	}
	if record.TransactionID != "original-transaction-1" || record.Environment != "production" ||
		record.OfferTransactionID != "offer-transaction-1" || record.OfferIdentifier != "friends-september" || record.OfferType != 3 {
		t.Fatalf("unexpected stored entitlement: %+v", record)
	}
	if binder.keyID != "key-1" || binder.transactionID != "original-transaction-1" {
		t.Fatalf("transaction was not bound to the attested key: %+v", binder)
	}
}

func TestProductionSyncRejectsTransactionAlreadyBoundToAnotherPurchase(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	service := NewProductionService(NewMemoryStore(), &fakeSubscriptionResolver{state: SubscriptionState{
		OriginalTransactionID: "original-transaction-2",
		Environment:           "Production",
		ExpiresAt:             now.Add(30 * 24 * time.Hour),
	}}, func() time.Time { return now }).WithTransactionBinder(&fakeTransactionBinder{
		err: attestation.ErrTransactionBindingConflict,
	})

	_, err := service.Sync(
		context.Background(),
		attestation.Principal{KeyID: "key-1", DeviceID: "device-1"},
		"signed-transaction",
	)
	if !errors.Is(err, ErrProductionSyncDenied) {
		t.Fatalf("expected permanent binding denial, got %v", err)
	}
}

func TestNotificationUpdateIsIdempotentAcrossBoundDevices(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	for _, keyID := range []string{"key-1", "key-2"} {
		if err := store.Upsert(ctx, Record{
			KeyID:         keyID,
			TransactionID: "original-transaction-1",
			Environment:   "production",
			ExpiresAt:     time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("seed entitlement: %v", err)
		}
	}
	newExpiry := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	applied, err := store.ApplyNotification(
		ctx,
		NotificationState{NotificationUUID: "notification-uuid-1", OriginalTransactionID: "original-transaction-1", Environment: "production", ExpiresAt: newExpiry},
	)
	if err != nil || !applied {
		t.Fatalf("apply notification: applied=%v err=%v", applied, err)
	}
	applied, err = store.ApplyNotification(
		ctx,
		NotificationState{NotificationUUID: "notification-uuid-1", OriginalTransactionID: "original-transaction-1", Environment: "production", ExpiresAt: newExpiry.Add(24 * time.Hour)},
	)
	if err != nil || applied {
		t.Fatalf("duplicate notification: applied=%v err=%v", applied, err)
	}
	for _, keyID := range []string{"key-1", "key-2"} {
		record, ok, getErr := store.Get(ctx, keyID)
		if getErr != nil || !ok || !record.ExpiresAt.Equal(newExpiry) {
			t.Fatalf("unexpected entitlement for %s: record=%+v ok=%v err=%v", keyID, record, ok, getErr)
		}
	}
}

func TestNotificationServiceAppliesVerifiedStateOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Upsert(ctx, Record{
		KeyID:         "key-1",
		TransactionID: "original-transaction-1",
		Environment:   "production",
		ExpiresAt:     time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed entitlement: %v", err)
	}
	expiredAt := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	resolver := fakeNotificationResolver{state: NotificationState{
		NotificationUUID:      "notification-uuid-1",
		OriginalTransactionID: "original-transaction-1",
		Environment:           "production",
		ExpiresAt:             expiredAt,
	}}
	service := NewNotificationService(store, resolver)

	applied, err := service.Process(ctx, "signed-notification")
	if err != nil || !applied {
		t.Fatalf("process notification: applied=%v err=%v", applied, err)
	}
	applied, err = service.Process(ctx, "signed-notification")
	if err != nil || applied {
		t.Fatalf("duplicate notification: applied=%v err=%v", applied, err)
	}
	record, ok, err := store.Get(ctx, "key-1")
	if err != nil || !ok || !record.ExpiresAt.Equal(expiredAt) {
		t.Fatalf("unexpected entitlement: record=%+v ok=%v err=%v", record, ok, err)
	}
}

type fakeSubscriptionResolver struct {
	state SubscriptionState
	err   error
}

type fakeTransactionBinder struct {
	keyID         string
	transactionID string
	err           error
}

func (binder *fakeTransactionBinder) BindTransaction(
	_ context.Context,
	keyID string,
	transactionID string,
) error {
	binder.keyID = keyID
	binder.transactionID = transactionID
	return binder.err
}

type fakeNotificationResolver struct {
	state NotificationState
	err   error
}

func (resolver fakeNotificationResolver) ResolveNotification(
	context.Context,
	string,
) (NotificationState, error) {
	return resolver.state, resolver.err
}

func (resolver *fakeSubscriptionResolver) Resolve(
	context.Context,
	string,
) (SubscriptionState, error) {
	return resolver.state, resolver.err
}
