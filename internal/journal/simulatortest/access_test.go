package simulatortest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
	"github.com/tellyouwhat/backend/internal/privacy"
)

func TestSimulatorAccessUsesSeparateSubjectAndRealDeviceScopedConsent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	entitlements := entitlement.NewMemoryStore()
	consents := privacy.NewService(privacy.NewMemoryRepository(), nil, nil, func() time.Time { return f.now })
	normal := &readaloud.Access{Entitlements: entitlements, Consents: consents, JournalCharacters: 999, PreviewCharacters: 100, Now: func() time.Time { return f.now }}
	a := Access{Simulator: f.s, Normal: normal}
	key := f.r.Credentials[0].KeyID
	grant, err := a.Grant(ctx, key, "journal")
	if err != nil || grant.Subject.ID != "simulator:"+f.r.Credentials[0].SubjectID || grant.Limit != 10000 {
		t.Fatal("incorrect fixture account", grant, err)
	}
	second := f.r.Credentials[0]
	second.KeyID = keyPrefix + uuid.NewString()
	f.r.Credentials = append(f.r.Credentials, second)
	f.write(t)
	other, err := a.Grant(ctx, second.KeyID, "journal")
	if err != nil || other.Subject != grant.Subject || other.Start != grant.Start || other.End != grant.End {
		t.Fatal("second key reset shared character allowance", other, err)
	}
	if err := a.Authorize(ctx, readaloud.Subject{AppID: "journal", ID: key}, "journal"); err != readaloud.ErrConsentRequired {
		t.Fatal("fixture subscription bypassed consent", err)
	}
	if err := a.Authorize(ctx, readaloud.Subject{AppID: "journal", ID: key}, "preview"); err != nil {
		t.Fatal("public fixed preview required private consent", err)
	}
	p := attestation.Principal{AppID: "journal", KeyID: key, DeviceID: key}
	consent := privacy.Consent{Scope: privacy.JournalReadAloudScope, DocumentVersion: privacy.JournalReadAloudDocumentVersion, Granted: true}
	if _, err := consents.RecordConsents(ctx, p, []privacy.Consent{consent}); err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(ctx, readaloud.Subject{AppID: "journal", ID: key}, "journal"); err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(ctx, readaloud.Subject{AppID: "journal", ID: second.KeyID}, "journal"); err != readaloud.ErrConsentRequired {
		t.Fatal("consent leaked across test devices", err)
	}
	consent.Granted = false
	if _, err := consents.RecordConsents(ctx, p, []privacy.Consent{consent}); err != nil {
		t.Fatal(err)
	}
	if err := a.Consent(ctx, key, "journal"); err != readaloud.ErrConsentRequired {
		t.Fatal("revoked consent still allowed a stream", err)
	}
	preview, err := a.Grant(ctx, key, "preview")
	if err != nil || preview.Limit != 1000 || preview.Start.Hour() != 0 || preview.End.Sub(preview.Start) != 24*time.Hour {
		t.Fatal("preview did not use independent daily allowance", preview, err)
	}
	if _, ok, err := entitlements.Get(ctx, key); err != nil || ok {
		t.Fatal("simulator forged an Apple purchase record", err)
	}
	real := entitlement.Record{KeyID: "real-apple-key", TransactionID: "verified", Environment: "sandbox", StartedAt: f.now.Add(-time.Hour), ExpiresAt: f.now.Add(time.Hour)}
	if err := entitlements.Upsert(ctx, real); err != nil {
		t.Fatal(err)
	}
	actual, err := a.Grant(ctx, real.KeyID, "journal")
	expected, normalErr := normal.Grant(ctx, real.KeyID, "journal")
	if err != nil || normalErr != nil || actual != expected || actual.Subject == grant.Subject {
		t.Fatal("normal Apple authorization changed", actual, err)
	}
	if err := a.Authorize(ctx, readaloud.Subject{AppID: "health", ID: key}, "preview"); err != readaloud.ErrInput {
		t.Fatal("cross App access", err)
	}
	if _, err := a.Grant(ctx, key, "unknown"); err != readaloud.ErrInput {
		t.Fatal("unknown purpose granted", err)
	}
	if _, err := a.Grant(ctx, "unprovisioned-key", "journal"); err != readaloud.ErrSubscriptionRequired {
		t.Fatal("unprovisioned identity got test access", err)
	}
}

func TestSimulatorSubscriptionFixturesAndRevocation(t *testing.T) {
	for _, name := range []string{"none", "expired", "future", "revoked", "expired credential"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			key := f.r.Credentials[0].KeyID
			c := &f.r.Credentials[0]
			want := readaloud.ErrSubscriptionRequired
			switch name {
			case "none":
				c.SubscriptionStartedAt = nil
				c.SubscriptionExpiresAt = nil
			case "expired":
				end := f.now.Add(-time.Minute)
				c.SubscriptionExpiresAt = &end
			case "future":
				start := f.now.Add(time.Minute)
				c.SubscriptionStartedAt = &start
				want = readaloud.ErrSubscriptionSync
			case "revoked":
				c.Revoked = true
				want = readaloud.ErrAccessUnavailable
			case "expired credential":
				f.now = c.ExpiresAt
				want = readaloud.ErrAccessUnavailable
			}
			f.write(t)
			a := Access{Simulator: f.s, Normal: &readaloud.Access{}}
			if _, err := a.Grant(ctx, key, "journal"); !errors.Is(err, want) {
				t.Fatal("wrong fixture grant", err)
			}
			if name == "expired" {
				if _, err := a.Subject(ctx, key); err != nil {
					t.Fatal("expired subscription lost cancellation access", err)
				}
			}
			if name == "revoked" || name == "expired credential" {
				if err := a.Consent(ctx, key, "preview"); err != readaloud.ErrAccessUnavailable {
					t.Fatal("in-flight preview outlived credential", err)
				}
			}
		})
	}
}
