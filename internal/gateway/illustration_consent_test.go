package gateway

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
)

func TestImageConsentHTTPGrantRevokeAndAppIsolation(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	repository := privacy.NewMemoryRepository()
	consentService := privacy.NewService(repository, nil, nil, func() time.Time { return now })
	journal := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: "journal"}, Privacy: consentService})
	health := New(Dependencies{App: appregistry.App{ID: appregistry.Health}, Authenticator: fakeAuthenticator{appID: "health"}, Privacy: consentService})
	record := func(server *Server, scope, version string, granted bool, want int) {
		t.Helper()
		response := httptest.NewRecorder()
		body := fmt.Sprintf(`{"consents":[{"scope":%q,"documentVersion":%q,"granted":%t}]}`, scope, version, granted)
		server.Router().ServeHTTP(response, authorizedRequest("POST", "/v1/privacy/consents", body))
		if response.Code != want {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
	}
	keys := attestation.NewMemoryKeyStore()
	keys.Put(attestation.RegisteredKey{AppID: "journal", KeyID: "valid-key", TransactionID: "subscription"})
	subscriptions := entitlement.NewMemoryStore()
	if err := subscriptions.Upsert(ctx, entitlement.Record{KeyID: "valid-key", TransactionID: "subscription", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	policy := &illustration.AccessPolicy{Keys: keys, Subscriptions: subscriptions, Consents: repository, MonthlyLimit: 5}
	allowed := func(want bool) {
		t.Helper()
		_, _, err := policy.ImageAllowance(ctx, "valid-key", now)
		if (err == nil) != want {
			t.Fatalf("image allowance: %v", err)
		}
	}
	record(journal, privacy.ManagedAIScope, privacy.AIDocumentVersion, true, 200)
	allowed(false)
	record(health, privacy.JournalIllustrationScope, privacy.JournalIllustrationDocumentVersion, true, 422)
	allowed(false)
	record(journal, privacy.JournalIllustrationScope, "old-version", true, 422)
	allowed(false)
	record(journal, privacy.JournalIllustrationScope, privacy.JournalIllustrationDocumentVersion, true, 200)
	allowed(true)
	// No illustration runtime is configured, yet withdrawal must remain usable.
	record(journal, privacy.JournalIllustrationScope, privacy.JournalIllustrationDocumentVersion, false, 200)
	allowed(false)
	textAllowed, err := repository.HasGrantedConsents(ctx, "valid-key", []privacy.Consent{{Scope: privacy.ManagedAIScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true}})
	if err != nil || !textAllowed {
		t.Fatal("image revocation changed text consent")
	}
}
