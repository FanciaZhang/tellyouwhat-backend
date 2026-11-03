package appstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTransactionAppAccountTokenIsCoveredByAppleSignature(t *testing.T) {
	now := time.Now().UTC()
	owner := "715c08fd-df9d-4f28-980b-57e21bbeb0b8"
	payload := transactionPayload{AppAccountToken: owner, BundleID: "cn.tellyouwhat.albums", ProductID: "albums.cloud.50gb.annual", Environment: "Sandbox",
		OriginalTransactionID: "10000001", TransactionID: "10000002", OriginalPurchaseDate: now.Add(-time.Hour).UnixMilli(), PurchaseDate: now.Add(-time.Hour).UnixMilli(), ExpiresDate: now.Add(time.Hour).UnixMilli(), SignedDate: now.UnixMilli()}
	fixture := newSignedTransactionFixture(t, now, payload)
	v := NewTransactionVerifier(VerifierConfig{Roots: fixture.roots, BundleID: payload.BundleID, Environment: "Sandbox", ProductID: payload.ProductID, Now: func() time.Time { return now }})
	transaction, err := v.VerifyTransaction(fixture.signed)
	if err != nil || transaction.AppAccountToken != owner {
		t.Fatal("signed account association lost", err)
	}
	segments := strings.Split(fixture.signed, ".")
	// Replace the payload without re-signing the account UUID.
	payload.AppAccountToken = "7300f2ba-5896-41ee-af12-b93dd6485adc"
	forged := fixture.signPayload(t, payload)
	forgedSegments := strings.Split(forged, ".")
	segments[1] = forgedSegments[1]
	if _, err := v.VerifyTransaction(strings.Join(segments, ".")); !errors.Is(err, ErrInvalidSignedData) {
		t.Fatal("account association could be tampered")
	}
}

func TestAPIClientAllStatusesRetainsInactiveEvidence(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired-retrying-revoked", true: "unknown-status-refused"}[bad], func(t *testing.T) {
			key := newTestECKey(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RawQuery != "" || r.URL.Path != "/inApps/v1/subscriptions/transaction" {
					t.Error("all-status request was filtered")
				}
				transactions := []map[string]any{}
				for _, status := range []int{1, 2, 3, 4, 5} {
					transactions = append(transactions, map[string]any{"status": status, "signedTransactionInfo": "signed"})
				}
				if bad {
					transactions = append(transactions, map[string]any{"status": 6, "signedTransactionInfo": "signed"})
				}
				json.NewEncoder(w).Encode(map[string]any{"environment": "Sandbox", "bundleId": "cn.tellyouwhat.albums", "data": []any{map[string]any{"lastTransactions": transactions}}})
			}))
			defer server.Close()
			client := NewAPIClient(APIClientConfig{BaseURL: server.URL, KeyID: "key", IssuerID: "issuer", BundleID: "cn.tellyouwhat.albums", Environment: "Sandbox", SigningKey: key, HTTPClient: server.Client()})
			states, err := client.AllSubscriptionStatuses(context.Background(), "transaction")
			if bad {
				if !errors.Is(err, ErrInvalidAPIResponse) {
					t.Fatal("unknown status accepted")
				}
				return
			}
			if err != nil || len(states) != 5 {
				t.Fatal("inactive evidence discarded", err)
			}
		})
	}
}
