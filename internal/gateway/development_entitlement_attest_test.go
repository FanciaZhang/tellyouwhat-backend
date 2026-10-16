package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/testutil/appattest"
)

func TestDevelopmentAccessUsesVerifiedDeviceWithoutManualSecretHTTP(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	client := appattest.New(t, "TEAM.cn.tellyouwhat.healthapp.dev", now)
	nonces, keys := attestation.NewMemoryNonceStore(), attestation.NewMemoryKeyStore()
	store := entitlement.NewMemoryStore()
	server := newTestServer()
	server.now = clock
	server.enrollment = attestation.NewEnrollmentService(attestation.EnrollmentConfig{
		AppID: "health", Environment: attestation.EnvironmentProduction, AllowedBuilds: map[string]struct{}{"4": {}},
	}, nonces, keys, attestation.NewAppleAttestationVerifier("TEAM", "cn.tellyouwhat.healthapp.dev", attestation.EnvironmentProduction, client.Roots), clock)
	server.authenticator = attestation.NewService(nonces, keys, attestation.NewAppleAssertionVerifier("TEAM", "cn.tellyouwhat.healthapp.dev"), clock).RequireEnvironment(attestation.EnvironmentProduction)
	server.activator = entitlement.NewDevelopmentService("health", store, 30*24*time.Hour, clock)
	server.entitlements = entitlement.NewChecker(store, clock)
	router := server.newHTTPRouter()
	serve := func(request *http.Request, expected int) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("%s: got %d, want %d: %s", request.URL.Path, response.Code, expected, response.Body.String())
		}
		return response
	}
	challenge := func() string {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"keyID": client.KeyID})
		request := httptest.NewRequest(http.MethodPost, "/v1/attest/challenges", bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		response := serve(request, 201)
		var output struct {
			Challenge string `json:"challenge"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &output); err != nil || output.Challenge == "" {
			t.Fatal("missing enrollment challenge")
		}
		return output.Challenge
	}
	path := "/v1/dev/entitlements/activate"
	unauthenticated := httptest.NewRequest(http.MethodPost, path, nil)
	unauthenticated.Header.Set("X-Tellyouwhat-Request-ID", uuid.NewString())
	unauthenticated.Header.Set("X-Tellyouwhat-Dev-Activation", "app-attest")
	serve(unauthenticated, 401)
	value := challenge()
	registration := map[string]string{"keyID": client.KeyID, "challenge": value,
		"attestation": base64.StdEncoding.EncodeToString(client.Attestation(t, value, "production")), "build": "4", "activationSecret": ""}
	register := func(expected int) {
		raw, _ := json.Marshal(registration)
		request := httptest.NewRequest(http.MethodPost, "/v1/attest/keys", bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		serve(request, expected)
	}
	registration["build"] = "unapproved"
	register(401)
	registration["build"] = "4"
	register(201)
	var counter uint32
	request := func(method, target string) *http.Request {
		counter++
		r := httptest.NewRequest(method, target, nil)
		r.Header.Set("X-Tellyouwhat-Request-ID", uuid.NewString())
		r.Header.Set("X-Tellyouwhat-Nonce", challenge())
		r.Header.Set("X-Tellyouwhat-Timestamp", now.Format(time.RFC3339))
		if target == path {
			r.Header.Set("X-Tellyouwhat-Dev-Activation", "app-attest")
		}
		client.Assert(t, r, nil, client.AppStoreData(t, counter))
		return r
	}
	activation := request(http.MethodPost, path)
	serve(activation, 200)
	serve(activation, 409)
	record, found, err := store.Get(context.Background(), client.KeyID)
	if err != nil || !found || record.Environment != "development" || record.TransactionID != "" || !record.ExpiresAt.Equal(now.Add(30*24*time.Hour)) {
		t.Fatal("development access was not separately bounded")
	}
	quota := serve(request(http.MethodGet, "/v1/ai/quota"), 200)
	if !strings.Contains(quota.Body.String(), `"plan":"managed_subscription"`) {
		t.Fatal("verified developer access not applied")
	}
	// Production installs no development activator, regardless of client headers.
	server.activator = nil
	serve(request(http.MethodPost, path), 404)
}
