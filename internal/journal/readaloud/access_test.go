package readaloud

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/privacy"
)

func TestReadAloudAccessSharesPurchaseQuotaAndKeepsDeviceConsentIndependent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	store := entitlement.NewMemoryStore()
	consents := privacy.NewService(privacy.NewMemoryRepository(), nil, nil, func() time.Time { return now })
	r := entitlement.Record{TransactionID: "verified-purchase", Environment: "sandbox", StartedAt: time.Date(2026, 1, 31, 8, 10, 0, 0, time.UTC), ExpiresAt: now.AddDate(0, 1, 0)}
	for _, key := range []string{"device-one", "device-two"} {
		r.KeyID = key
		if err := store.Upsert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	a := Access{Entitlements: store, Consents: consents, JournalCharacters: 10000, PreviewCharacters: 1000, Now: func() time.Time { return now }}
	one, err := a.Grant(ctx, "device-one", "journal")
	if err != nil {
		t.Fatal(err)
	}
	two, err := a.Grant(ctx, "device-two", "journal")
	if err != nil || one.Subject != two.Subject {
		t.Fatal("reinstall/device change reset purchase allowance", err)
	}
	if one.Start != time.Date(2026, 2, 28, 8, 10, 0, 0, time.UTC) || one.End != time.Date(2026, 3, 31, 8, 10, 0, 0, time.UTC) {
		t.Fatal("billing anchor drift", one)
	}
	if err := a.Authorize(ctx, Subject{"journal", "device-one"}, "journal"); err != ErrConsentRequired {
		t.Fatal("old authorization implied TTS consent", err)
	}
	if err := a.Authorize(ctx, Subject{"journal", "device-two"}, "preview"); err != nil {
		t.Fatal("public server sample required private upload consent", err)
	}
	p := attestation.Principal{AppID: "journal", KeyID: "device-one", DeviceID: "one"}
	if _, err := consents.RecordConsents(ctx, p, privacy.JournalCloudAIConsents(true)); err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(ctx, Subject{"journal", "device-one"}, "journal"); err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(ctx, Subject{"journal", "device-two"}, "journal"); err != ErrConsentRequired {
		t.Fatal("device consent crossed keys", err)
	}
	r.KeyID = "production-device"
	r.Environment = "production"
	if err := store.Upsert(ctx, r); err != nil {
		t.Fatal(err)
	}
	production, err := a.Subject(ctx, r.KeyID)
	if err != nil || production == one.Subject {
		t.Fatal("sandbox allowance crossed environments", err)
	}
	now = now.AddDate(0, 2, 0)
	if _, err := a.Grant(ctx, "device-one", "journal"); err != ErrSubscriptionRequired {
		t.Fatal("expired entitlement synthesized", err)
	}
	if subject, err := a.Subject(ctx, "device-one"); err != nil || subject != one.Subject {
		t.Fatal("expired purchase lost cancellation access", err)
	}
}
func TestReadAloudAccessRejectsUnverifiedMissingPeriodsAndUsesUTCDailyPreview(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 2, 30, 0, 0, time.FixedZone("UTC+8", 8*3600))
	store := entitlement.NewMemoryStore()
	r := entitlement.Record{KeyID: "key", TransactionID: "purchase", Environment: "development", StartedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	_ = store.Upsert(ctx, r)
	a := Access{Entitlements: store, JournalCharacters: 10000, PreviewCharacters: 1000, Now: func() time.Time { return now }}
	if _, err := a.Grant(ctx, "key", "journal"); err != ErrSubscriptionRequired {
		t.Fatal("development record impersonated verified purchase", err)
	}
	r.Environment = "sandbox"
	r.StartedAt = time.Time{}
	_ = store.Upsert(ctx, r)
	if _, err := a.Grant(ctx, "key", "journal"); err != ErrSubscriptionSync {
		t.Fatal("missing period silently reset quota", err)
	}
	r.StartedAt = now.Add(-time.Hour)
	_ = store.Upsert(ctx, r)
	preview, err := a.Grant(ctx, "key", "preview")
	if err != nil || preview.Start != time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC) || preview.End.Sub(preview.Start) != 24*time.Hour || preview.Limit != 1000 {
		t.Fatal("preview did not use UTC daily allowance", preview, err)
	}
}

type failedEntitlements struct{}

func (failedEntitlements) Get(context.Context, string) (entitlement.Record, bool, error) {
	return entitlement.Record{}, false, errors.New("private database credentials")
}
func TestReadAloudAccessDoesNotExposeRepositoryDetails(t *testing.T) {
	a := Access{Entitlements: failedEntitlements{}}
	if _, err := a.Subject(context.Background(), "key"); err != ErrAccessUnavailable {
		t.Fatal("unsafe authorization error", err)
	}
}

func TestDevelopmentAccessCannotEnterProductionReadAloud(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := entitlement.NewMemoryStore()
	_ = store.Upsert(ctx, entitlement.Record{KeyID: "key", Environment: "development", StartedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)})
	access := Access{Entitlements: store, JournalCharacters: 1000, PreviewCharacters: 100, Now: func() time.Time { return now }}
	if _, err := access.Grant(ctx, "key", "preview"); err != ErrSubscriptionRequired {
		t.Fatal("production accepted development access", err)
	}
	access.Development = true
	grant, err := access.Grant(ctx, "key", "preview")
	if err != nil || !strings.HasPrefix(grant.Subject.ID, "development:") {
		t.Fatal("explicit developer reader unavailable", err)
	}
}
