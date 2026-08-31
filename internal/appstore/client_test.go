// Created by OpenAI Codex on 2026-08-03.

package appstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseSigningKeyPEMAcceptsAppStoreConnectPKCS8Key(t *testing.T) {
	t.Parallel()

	key := newTestECKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS8 key: %v", err)
	}
	parsed, err := ParseSigningKeyPEM(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		t.Fatalf("parse signing key: %v", err)
	}
	if !parsed.Equal(key) {
		t.Fatal("parsed signing key differs")
	}
}

func TestAPIClientRequestsActiveAndGraceStatusesWithSignedBearerToken(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	signingKey := newTestECKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/inApps/v1/subscriptions/transaction-2" ||
			request.URL.Query()["status"][0] != "1" || request.URL.Query()["status"][1] != "4" {
			t.Errorf("unexpected status request: %s", request.URL.String())
		}
		token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		assertTestBearerClaims(t, token, "issuer-id", "cn.tellyouwhat.healthapp", now, &signingKey.PublicKey)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
            "environment":"Production",
            "bundleId":"cn.tellyouwhat.healthapp",
            "appAppleId":1234567890,
            "data":[{"subscriptionGroupIdentifier":"group-1","lastTransactions":[
                {"status":1,"signedTransactionInfo":"signed-current","signedRenewalInfo":"signed-renewal"}
            ]}]
        }`))
	}))
	defer server.Close()
	client := NewAPIClient(APIClientConfig{
		BaseURL:     server.URL,
		KeyID:       "key-id",
		IssuerID:    "issuer-id",
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		SigningKey:  signingKey,
		HTTPClient:  server.Client(),
		Now:         func() time.Time { return now },
	})

	statuses, err := client.CurrentTransactions(context.Background(), "transaction-2")
	if err != nil {
		t.Fatalf("get current transactions: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Status != 1 ||
		statuses[0].SignedTransaction != "signed-current" ||
		statuses[0].SignedRenewal != "signed-renewal" {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}
}

func TestAPIClientTreatsRejectedTransactionIdentifierAsNotFound(t *testing.T) {
	t.Parallel()

	signingKey := newTestECKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	client := NewAPIClient(APIClientConfig{
		BaseURL:     server.URL,
		KeyID:       "key-id",
		IssuerID:    "issuer-id",
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		SigningKey:  signingKey,
		HTTPClient:  server.Client(),
	})

	_, err := client.CurrentTransactions(context.Background(), "unknown-transaction")
	if !errors.Is(err, ErrTransactionNotFound) {
		t.Fatalf("expected permanent transaction rejection, got %v", err)
	}
}

func assertTestBearerClaims(
	t *testing.T,
	token string,
	issuerID string,
	bundleID string,
	now time.Time,
	publicKey *ecdsa.PublicKey,
) {
	t.Helper()
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		t.Fatalf("invalid bearer token: %q", token)
	}
	header := decodeTestJWTObject(t, segments[0])
	payload := decodeTestJWTObject(t, segments[1])
	if header["alg"] != "ES256" || header["kid"] != "key-id" || header["typ"] != "JWT" {
		t.Fatalf("unexpected bearer header: %+v", header)
	}
	if payload["iss"] != issuerID || payload["aud"] != "appstoreconnect-v1" || payload["bid"] != bundleID {
		t.Fatalf("unexpected bearer payload: %+v", payload)
	}
	issuedAt, ok := payload["iat"].(float64)
	if !ok || int64(issuedAt) != now.Unix() {
		t.Fatalf("unexpected issued-at: %+v", payload["iat"])
	}
	verifyTestJWTSignature(t, publicKey, segments)
}

func decodeTestJWTObject(t *testing.T, segment string) map[string]any {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decode JWT segment: %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("decode JWT JSON: %v", err)
	}
	return object
}

func verifyTestJWTSignature(t *testing.T, publicKey *ecdsa.PublicKey, segments []string) {
	t.Helper()
	signature, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil || len(signature) != 64 {
		t.Fatalf("invalid JWT signature: len=%d err=%v", len(signature), err)
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(publicKey, digest[:], r, s) {
		t.Fatal("bearer JWT signature did not verify")
	}
}

