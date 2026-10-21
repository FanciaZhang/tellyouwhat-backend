package appstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSandboxRetryableStatusRequiresFreshVerifiedActiveTransaction(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	revoked := now.UnixMilli()
	for _, test := range []struct {
		name                             string
		environment                      string
		statusHTTP, appleError, infoHTTP int
		mutate                           func(*transactionPayload)
		invalidSignature, emptyInfo      bool
		wantError                        error
		wantInfoCalls                    int32
	}{
		{name: "active", wantInfoCalls: 1},
		{name: "expired", mutate: func(p *transactionPayload) { p.ExpiresDate = now.UnixMilli() }, wantError: ErrSubscriptionInactive, wantInfoCalls: 1},
		{name: "revoked", mutate: func(p *transactionPayload) { p.RevocationDate = &revoked }, wantError: ErrSubscriptionInactive, wantInfoCalls: 1},
		{name: "upgraded", mutate: func(p *transactionPayload) { p.IsUpgraded = true }, wantError: ErrSubscriptionInactive, wantInfoCalls: 1},
		{name: "different chain", mutate: func(p *transactionPayload) { p.OriginalTransactionID = "other-chain" }, wantError: ErrInvalidSignedData, wantInfoCalls: 1},
		{name: "different transaction", mutate: func(p *transactionPayload) { p.TransactionID = "other-transaction" }, wantError: ErrInvalidSignedData, wantInfoCalls: 1},
		{name: "different product", mutate: func(p *transactionPayload) { p.ProductID = "journal.ai.subscription.monthly.dev" }, wantError: ErrInvalidSignedData, wantInfoCalls: 1},
		{name: "different bundle", mutate: func(p *transactionPayload) { p.BundleID = "other.bundle" }, wantError: ErrInvalidSignedData, wantInfoCalls: 1},
		{name: "different environment", mutate: func(p *transactionPayload) { p.Environment = "Production" }, wantError: ErrInvalidSignedData, wantInfoCalls: 1},
		{name: "invalid signature", invalidSignature: true, wantError: ErrInvalidSignedData, wantInfoCalls: 1},
		{name: "empty fresh response", emptyInfo: true, wantError: ErrStatusUnavailable, wantInfoCalls: 1},
		{name: "fresh API unavailable", infoHTTP: 503, wantError: ErrStatusUnavailable, wantInfoCalls: 1},
		{name: "production has no fallback", environment: "Production", wantError: ErrStatusUnavailable},
		{name: "unauthorized has no fallback", statusHTTP: 401, wantError: ErrStatusUnavailable},
		{name: "other server error has no fallback", appleError: 5000000, wantError: ErrStatusUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := test.environment
			if environment == "" {
				environment = "Sandbox"
			}
			payload := transactionPayload{OriginalTransactionID: "original-chain", TransactionID: "current-renewal", BundleID: "cn.tellyouwhat.journalapp.dev", ProductID: "journal.ai.subscription.annual.dev", Environment: environment, ExpiresDate: now.Add(time.Hour).UnixMilli(), SignedDate: now.UnixMilli()}
			fixture := newSignedTransactionFixture(t, now, payload)
			if test.mutate != nil {
				test.mutate(&payload)
			}
			fresh := fixture.signPayload(t, payload)
			if test.invalidSignature {
				fresh = "invalid.signature.payload"
			}
			var infoCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/inApps/v1/subscriptions/original-chain":
					status := test.statusHTTP
					if status == 0 {
						status = 500
					}
					code := test.appleError
					if code == 0 {
						code = 5000001
					}
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]any{"errorCode": code})
				case "/inApps/v1/transactions/current-renewal":
					infoCalls.Add(1)
					if test.infoHTTP != 0 {
						w.WriteHeader(test.infoHTTP)
						return
					}
					if test.emptyInfo {
						_ = json.NewEncoder(w).Encode(map[string]any{})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"signedTransactionInfo": fresh})
				default:
					t.Errorf("unexpected Apple request path: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			api := NewAPIClient(APIClientConfig{BaseURL: server.URL, KeyID: "fixture-key", IssuerID: "fixture-issuer", BundleID: payload.BundleID, Environment: environment, SigningKey: newTestECKey(t), HTTPClient: server.Client(), Now: func() time.Time { return now }})
			verifier := NewTransactionVerifier(VerifierConfig{Roots: fixture.roots, BundleID: "cn.tellyouwhat.journalapp.dev", Environment: environment, ProductIDs: []string{"journal.ai.subscription.annual.dev", "journal.ai.subscription.monthly.dev"}, Now: func() time.Time { return now }})
			state, err := NewSubscriptionResolver(verifier, api, func() time.Time { return now }).Resolve(context.Background(), fixture.signed)
			if test.wantError == nil {
				if err != nil || state.TransactionID != "current-renewal" || !state.ExpiresAt.Equal(now.Add(time.Hour)) {
					t.Fatalf("fresh transaction did not activate: %+v, %v", state, err)
				}
			} else if !errors.Is(err, test.wantError) {
				t.Fatalf("expected %v, got state %+v and error %v", test.wantError, state, err)
			}
			if infoCalls.Load() != test.wantInfoCalls {
				t.Fatalf("fresh Apple queries: %d; expected %d", infoCalls.Load(), test.wantInfoCalls)
			}
		})
	}
}
