package illustration

import (
	"context"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/privacy"
	"testing"
	"time"
)

type reservationFunc func(context.Context, Task) (bool, error)

func (f reservationFunc) HasImageReservation(ctx context.Context, task Task) (bool, error) {
	return f(ctx, task)
}

func TestAccessRequiresJournalSubscriptionAndExplicitImageConsent(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	keys := attestation.NewMemoryKeyStore()
	key := attestation.RegisteredKey{AppID: "journal", KeyID: "key", TransactionID: "subscription", DeviceID: "device"}
	keys.Put(key)
	subscriptions := entitlement.NewMemoryStore()
	record := entitlement.Record{KeyID: "key", TransactionID: "subscription", ExpiresAt: now.Add(time.Hour)}
	if err := subscriptions.Upsert(ctx, record); err != nil {
		t.Fatal(err)
	}
	consents := privacy.NewMemoryRepository()
	service := privacy.NewService(consents, nil, nil, func() time.Time { return now })
	principal := attestation.Principal{AppID: "journal", KeyID: "key", DeviceID: "device"}
	grant := func(scope, version string, allowed bool) {
		t.Helper()
		if _, err := service.RecordConsents(ctx, principal, []privacy.Consent{{Scope: scope, DocumentVersion: version, Granted: allowed}}); err != nil {
			t.Fatal(err)
		}
	}
	policy := &AccessPolicy{Keys: keys, Subscriptions: subscriptions, Consents: consents, MonthlyLimit: 5, Now: func() time.Time { return now }, Reservations: reservationFunc(func(context.Context, Task) (bool, error) { return true, nil })}
	grant(privacy.ManagedAIScope, privacy.AIDocumentVersion, true)
	if _, _, err := policy.ImageAllowance(ctx, "key", now); err == nil {
		t.Fatal("text consent authorized image upload")
	}
	grant(privacy.JournalIllustrationScope, privacy.JournalIllustrationDocumentVersion, true)
	account, limit, err := policy.ImageAllowance(ctx, "key", now)
	if err != nil || account != "subscription" || limit != 5 {
		t.Fatal("active allowance rejected")
	}
	task := Task{AppID: "journal", OwnerID: "key", BillingAccount: account, ConsentRevision: privacy.JournalIllustrationDocumentVersion, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := policy.Authorize(ctx, task); err != nil {
		t.Fatal(err)
	}
	grant(privacy.JournalIllustrationScope, privacy.JournalIllustrationDocumentVersion, false)
	if policy.Authorize(ctx, task) == nil {
		t.Fatal("revocation ignored")
	}
	grant(privacy.JournalIllustrationScope, privacy.JournalIllustrationDocumentVersion, true)
	policy.Reservations = reservationFunc(func(context.Context, Task) (bool, error) { return false, nil })
	if policy.Authorize(ctx, task) == nil {
		t.Fatal("unreserved dispatch")
	}
	task.State, task.Charge = TaskSucceeded, ChargeOneImage
	if err := policy.AuthorizeRead(ctx, task); err != nil {
		t.Fatal("settled result must not require a live reservation", err)
	}
	grant(privacy.JournalIllustrationScope, privacy.JournalIllustrationDocumentVersion, false)
	if policy.AuthorizeRead(ctx, task) == nil {
		t.Fatal("revoked completed result allowed")
	}
	grant(privacy.JournalIllustrationScope, privacy.JournalIllustrationDocumentVersion, true)
	record.ExpiresAt = now
	_ = subscriptions.Upsert(ctx, record)
	if _, _, err := policy.ImageAllowance(ctx, "key", now); err == nil {
		t.Fatal("expired subscription")
	}
	record.ExpiresAt = now.Add(time.Hour)
	record.TransactionID = "other"
	_ = subscriptions.Upsert(ctx, record)
	if _, _, err := policy.ImageAllowance(ctx, "key", now); err == nil {
		t.Fatal("subscription binding changed")
	}
	record.TransactionID = key.TransactionID
	_ = subscriptions.Upsert(ctx, record)
	key.AppID = "health"
	keys.Put(key)
	if _, _, err := policy.ImageAllowance(ctx, "key", now); err == nil {
		t.Fatal("health account authorized")
	}
}
