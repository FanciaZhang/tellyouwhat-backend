package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/observability"
	"github.com/tellyouwhat/backend/internal/testutil/appattest"
)

func TestAppStoreAttestationSubscriptionSyncAndQuotaHTTP(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	client := appattest.New(t, "TEAM.cn.tellyouwhat.healthapp", now)
	nonces, keys := attestation.NewMemoryNonceStore(), attestation.NewMemoryKeyStore()
	store := entitlement.NewMemoryStore()
	resolverCalls := 0
	resolver := entitlement.SubscriptionResolverFunc(func(_ context.Context, proof string) (entitlement.SubscriptionState, error) {
		resolverCalls++
		if proof != "synthetic-storekit-proof" {
			t.Fatal("request body binding did not preserve the transaction")
		}
		return entitlement.SubscriptionState{OriginalTransactionID: "synthetic-purchase", Environment: "production", ExpiresAt: now.Add(time.Hour)}, nil
	})
	server := newTestServer()
	server.now = clock
	server.enrollment = attestation.NewEnrollmentService(attestation.EnrollmentConfig{AppID: "health", Environment: attestation.EnvironmentProduction}, nonces, keys,
		attestation.NewAppleAttestationVerifier("TEAM", "cn.tellyouwhat.healthapp", attestation.EnvironmentProduction, client.Roots), clock)
	server.authenticator = attestation.NewService(nonces, keys, attestation.NewAppleAssertionVerifier("TEAM", "cn.tellyouwhat.healthapp"), clock).RequireEnvironment(attestation.EnvironmentProduction)
	server.productionEntitlement = entitlement.NewProductionService(store, resolver, clock).WithTransactionBinder(keys)
	server.entitlements = entitlement.NewChecker(store, clock)
	var logs bytes.Buffer
	server.httpMiddleware = observability.MiddlewareForApp(slog.New(slog.NewJSONHandler(&logs, nil)), "health")
	router := server.newHTTPRouter()
	serve := func(request *http.Request, status int) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s %s: status=%d body=%s", request.Method, request.URL.Path, response.Code, response.Body.String())
		}
		return response
	}
	post := func(path string, value any, status int) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return serve(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data)), status)
	}
	challenge := func() string {
		t.Helper()
		response := post("/v1/attest/challenges", map[string]any{"keyID": client.KeyID}, 201)
		var value struct {
			Challenge string `json:"challenge"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil || value.Challenge == "" {
			t.Fatal("invalid challenge response")
		}
		return value.Challenge
	}
	registrationChallenge := challenge()
	registration := map[string]any{"keyID": client.KeyID, "challenge": registrationChallenge,
		"attestation": base64.StdEncoding.EncodeToString(client.Attestation(t, registrationChallenge, "production")), "build": "1057", "activationSecret": ""}
	registered := post("/v1/attest/keys", registration, 201)
	// The initial successful response can be lost. Retrying the shipped client's
	// cached proof after the nonce TTL must retain the same server identity.
	now = now.Add(6 * time.Minute)
	recovered := post("/v1/attest/keys", registration, 201)
	if recovered.Body.String() != registered.Body.String() {
		t.Fatal("expired retry changed the registered identity")
	}
	var counter uint32
	request := func(method, path, body string, legacy bool) *http.Request {
		t.Helper()
		counter++
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Tellyouwhat-Request-ID", strings.ToUpper(uuid.NewString()))
		r.Header.Set("X-Tellyouwhat-Nonce", challenge())
		r.Header.Set("X-Tellyouwhat-Timestamp", now.Format(time.RFC3339))
		data := client.AppStoreData(t, counter)
		if legacy {
			data = client.AuthenticatorData(t, counter, 0x01, nil)
		} else if len(data) != 102 || data[32] != 0xc0 {
			t.Fatal("App Store fixture shape drifted")
		}
		client.Assert(t, r, []byte(body), data)
		return r
	}
	before := serve(request(http.MethodGet, "/v1/ai/quota", "", false), 200)
	if !strings.Contains(before.Body.String(), `"plan":"free"`) {
		t.Fatal("unverified subscription enabled managed quota")
	}
	body := "{\n  \"signedTransaction\": \"synthetic-storekit-proof\"\n}"
	serve(request(http.MethodPost, "/v1/entitlements/transactions", body, false), 200)
	for _, legacy := range []bool{false, true} {
		quota := serve(request(http.MethodGet, "/v1/ai/quota?timeZoneIdentifier=Asia%2FShanghai", "", legacy), 200)
		if !strings.Contains(quota.Body.String(), `"plan":"managed_subscription"`) {
			t.Fatal("verified subscription did not enable managed quota")
		}
	}
	// Full-stack tampering must fail before the StoreKit resolver is called.
	tampered := request(http.MethodPost, "/v1/entitlements/transactions", body, false)
	tampered.Body = io.NopCloser(strings.NewReader(`{"signedTransaction":"tampered-proof"}`))
	serve(tampered, 401)
	if resolverCalls != 1 {
		t.Fatal("invalid request reached subscription verification")
	}
	quotaRequest := request(http.MethodGet, "/v1/ai/quota", "", false)
	serve(quotaRequest, 200)
	serve(quotaRequest, 409)
	for _, private := range []string{client.KeyID, registrationChallenge, registration["attestation"].(string), quotaRequest.Header.Get("X-Tellyouwhat-Assertion"), quotaRequest.Header.Get("X-Tellyouwhat-Nonce"), "synthetic-storekit-proof"} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("private proof appeared in request logs")
		}
	}
}

func TestEnrollmentFailuresHaveBoundedPrivateDiagnosticsHTTP(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		expired     bool
		badProof    bool
		badBuild    bool
		status      int
		code, stage string
	}{
		{"expired pending registration", true, false, false, 401, "authentication_failed", "nonce"},
		{"invalid attestation", false, true, false, 401, "enrollment_denied", "attestation_verification"},
		{"build denied", false, false, true, 401, "enrollment_denied", "enrollment_policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			client := appattest.New(t, "TEAM.bundle", now)
			svc := attestation.NewEnrollmentService(attestation.EnrollmentConfig{AppID: "health", Environment: attestation.EnvironmentProduction, AllowedBuilds: map[string]struct{}{"1057": {}}},
				attestation.NewMemoryNonceStore(), attestation.NewMemoryKeyStore(), attestation.NewAppleAttestationVerifier("TEAM", "bundle", attestation.EnvironmentProduction, client.Roots), func() time.Time { return now })
			challenge, err := svc.IssueChallenge(context.Background(), client.KeyID)
			if err != nil {
				t.Fatal(err)
			}
			proof := client.Attestation(t, challenge.Value, "production")
			if tc.badProof {
				proof = []byte("private-invalid-proof")
			}
			build := "1057"
			if tc.badBuild {
				build = "unallowed-build"
			}
			if tc.expired {
				now = now.Add(6 * time.Minute)
			}
			body, err := json.Marshal(map[string]any{"keyID": client.KeyID, "challenge": challenge.Value, "attestation": base64.StdEncoding.EncodeToString(proof), "build": build, "activationSecret": ""})
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			server := New(Dependencies{Enrollment: svc, HTTPMiddleware: observability.MiddlewareForApp(slog.New(slog.NewJSONHandler(&logs, nil)), "health")})
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/attest/keys", bytes.NewReader(body)))
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.code) {
				t.Fatalf("public recovery contract changed: %d %s", response.Code, response.Body.String())
			}
			if !strings.Contains(logs.String(), `"authentication_failure_stage":"`+tc.stage+`"`) {
				t.Fatal("enrollment failure stage missing")
			}
			for _, private := range []string{client.KeyID, challenge.Value, base64.StdEncoding.EncodeToString(proof)} {
				if strings.Contains(logs.String(), private) || strings.Contains(response.Body.String(), private) {
					t.Fatal("enrollment proof leaked")
				}
			}
			if strings.Contains(response.Body.String(), "authentication_failure_stage") {
				t.Fatal("internal diagnostic exposed to client")
			}
		})
	}
}

func TestAttestationDependencyFailuresDoNotTriggerClientReenrollmentHTTP(t *testing.T) {
	t.Parallel()
	server := newTestServer()
	server.enrollment = attestation.NewEnrollmentService(attestation.EnrollmentConfig{AppID: "health", Environment: attestation.EnvironmentProduction}, nil, nil, nil, nil)
	server.authenticator = attestation.NewService(nil, nil, nil, nil)
	var logs bytes.Buffer
	server.httpMiddleware = observability.MiddlewareForApp(slog.New(slog.NewJSONHandler(&logs, nil)), "health")
	router := server.newHTTPRouter()
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/attest/challenges", `{"keyID":"synthetic-key"}`},
		{http.MethodPost, "/v1/attest/keys", `{"keyID":"synthetic-key","challenge":"synthetic-challenge","attestation":"YQ==","build":"1057","activationSecret":""}`},
		{http.MethodGet, "/v1/ai/quota", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			logs.Reset()
			response := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			request.Header.Set("X-Tellyouwhat-Request-ID", uuid.NewString())
			router.ServeHTTP(response, request)
			if response.Code != 503 || !strings.Contains(response.Body.String(), "attestation_unavailable") {
				t.Fatalf("infrastructure failure became a client authentication failure: %d %s", response.Code, response.Body.String())
			}
			if !strings.Contains(logs.String(), `"authentication_failure_stage":"dependencies"`) {
				t.Fatal("dependency diagnostic missing")
			}
		})
	}
}
