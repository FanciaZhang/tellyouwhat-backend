package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/gateway"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/platformhttpapi"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/storage/redisstore"
	"github.com/tellyouwhat/backend/internal/testutil"
	"github.com/tellyouwhat/backend/internal/testutil/appattest"
)

func allowAlbumBootstrap(next http.Handler) http.Handler { return next }

func TestAlbumAttestationRouterRejectsOtherPlatformOperationsBeforeDecoding(t *testing.T) {
	document, err := platformhttpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	handler := gateway.NewAttestationRouter(nil)
	checked := 0
	for path, item := range document.Paths.Map() {
		if strings.HasPrefix(path, "/v1/attest/") {
			continue
		}
		for method := range item.Operations() {
			for _, body := range []string{`{}`, `{broken`} {
				request := httptest.NewRequest(method, path, strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusNotFound {
					t.Errorf("%s %s: status=%d body=%s", method, path, response.Code, response.Body.String())
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-enrollment operations checked")
	}
}

func TestAlbumAppAttestHTTPBindsRequestWithoutGrantingAccountOrSubscription(t *testing.T) {
	for _, tc := range []struct{ bundle, storage string }{
		{"cn.tellyouwhat.albums.dev", "memory"}, {"cn.tellyouwhat.albums", "memory"},
		{"cn.tellyouwhat.albums.dev", "mysql-redis"}, {"cn.tellyouwhat.albums", "mysql-redis"},
	} {
		t.Run(tc.bundle+"/"+tc.storage, func(t *testing.T) {
			bundle := tc.bundle
			now := time.Now().UTC().Truncate(time.Second)
			client := appattest.New(t, "TEAM."+bundle, now)
			var nonces attestation.NonceStore = attestation.NewMemoryNonceStore()
			var keys interface {
				attestation.KeyStore
				attestation.EnrollmentKeyStore
			} = attestation.NewMemoryKeyStore()
			var issuedNonces []string
			if tc.storage == "mysql-redis" {
				redisURL := os.Getenv("REDIS_TEST_URL")
				if redisURL == "" {
					t.Skip("REDIS_TEST_URL required for real enrollment storage")
				}
				db := testutil.MySQL(t)
				redis, err := redisstore.Open(context.Background(), redisURL)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					defer redis.Close()
					var names []string
					for _, nonce := range issuedNonces {
						names = append(names, "platform:albums:attest:nonce:"+nonce)
					}
					if len(names) > 0 {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if err := redis.Del(ctx, names...).Err(); err != nil {
							t.Error("nonce fixture cleanup failed", err)
						}
					}
				})
				nonces = redisstore.NewNonceStore(redis, "albums")
				keys = mysqlstore.NewKeyRepository(db, "albums")
			}
			app := config.AppConfig{Registry: appregistry.App{ID: appregistry.Albums, TeamID: "TEAM", BundleID: bundle},
				AttestationEnvironment: attestation.EnvironmentProduction, AllowedBuilds: map[string]struct{}{"9": {}}}
			calls := 0
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer synthetic-account" {
					w.WriteHeader(401)
					return
				}
				// A device proof alone must never supply account or paid storage rights.
				if r.URL.Path == "/v1/albums/uploads" {
					w.WriteHeader(403)
					return
				}
				_, _ = io.Copy(w, r.Body)
			})
			handler := newAlbumAttestedHandler(app, nonces, keys, client.Roots, nil, next, allowAlbumBootstrap)
			serve := func(r *http.Request, status int) *httptest.ResponseRecorder {
				t.Helper()
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != status {
					t.Fatalf("%s %s = %d, want %d: %s", r.Method, r.URL, w.Code, status, w.Body.String())
				}
				if w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("proof responses must not be cached")
				}
				return w
			}
			post := func(path string, value any, status int) *httptest.ResponseRecorder {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("POST", path, bytes.NewReader(data))
				r.Header.Set("Content-Type", "application/json")
				return serve(r, status)
			}
			challenge := func() string {
				t.Helper()
				w := post("/v1/attest/challenges", map[string]string{"keyID": client.KeyID}, 201)
				var value struct{ Challenge string }
				if json.Unmarshal(w.Body.Bytes(), &value) != nil || value.Challenge == "" {
					t.Fatal("missing challenge")
				}
				issuedNonces = append(issuedNonces, value.Challenge)
				return value.Challenge
			}
			registrationNonce := challenge()
			registration := map[string]string{"keyID": client.KeyID, "challenge": registrationNonce,
				"attestation": base64.StdEncoding.EncodeToString(client.Attestation(t, registrationNonce, "production")), "build": "9", "activationSecret": ""}
			bad := map[string]string{}
			for k, v := range registration {
				bad[k] = v
			}
			bad["build"] = "8"
			post("/v1/attest/keys", bad, 401)
			registered := post("/v1/attest/keys", registration, 201)
			handler = newAlbumAttestedHandler(app, nonces, keys, client.Roots, nil, next, allowAlbumBootstrap)
			if recovered := post("/v1/attest/keys", registration, 201); recovered.Body.String() != registered.Body.String() {
				t.Fatal("lost enrollment reply changed the identity")
			}
			var counter uint32
			signed := func(method, path, body string) *http.Request {
				t.Helper()
				counter++
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer synthetic-account")
				id, nonce, timestamp := uuid.NewString(), challenge(), now.Format(time.RFC3339)
				bodyHash := sha256.Sum256([]byte(body))
				// Independent copy of the native newline binding, including the encoded query.
				digest := sha256.Sum256([]byte(strings.Join([]string{method, path, id, nonce, timestamp, hex.EncodeToString(bodyHash[:])}, "\n")))
				r.Header.Set("X-Tellyouwhat-Request-ID", strings.ToUpper(id))
				r.Header.Set("X-Tellyouwhat-Key-ID", client.KeyID)
				r.Header.Set("X-Tellyouwhat-Nonce", nonce)
				r.Header.Set("X-Tellyouwhat-Timestamp", timestamp)
				r.Header.Set("X-Tellyouwhat-Assertion", base64.StdEncoding.EncodeToString(client.Sign(t, client.AppStoreData(t, counter), digest[:])))
				return r
			}
			r := signed("POST", "/v1/albums/collections?limit=20&after=a%2Bb", `{"title":"synthetic"}`)
			if w := serve(r, 200); w.Body.String() != `{"title":"synthetic"}` {
				t.Fatal("body not preserved")
			}
			r.Body = io.NopCloser(strings.NewReader(`{"title":"synthetic"}`))
			handler = newAlbumAttestedHandler(app, nonces, keys, client.Roots, nil, next, allowAlbumBootstrap)
			serve(r, 409)
			for _, mutate := range []func(*http.Request){
				func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`{"title":"tampered"}`)) },
				func(r *http.Request) { r.URL.RawQuery = "limit=99" },
				func(r *http.Request) { r.Method = "DELETE" },
				func(r *http.Request) { r.URL.Path += "/other" },
				func(r *http.Request) { r.Header.Add("X-Tellyouwhat-Nonce", "ambiguous") },
				func(r *http.Request) { r.Header.Set("X-Tellyouwhat-Authentication", "simulator") },
				func(r *http.Request) { r.Header.Set("X-Tellyouwhat-Dev-Activation", "fixture") },
				func(r *http.Request) { r.Header.Set("X-Tellyouwhat-Request-ID", uuid.Nil.String()) },
				func(r *http.Request) {
					r.Header.Set("X-Tellyouwhat-Timestamp", now.Add(-time.Hour).Format(time.RFC3339))
				},
			} {
				r = signed("POST", "/v1/albums/collections?limit=20", `{}`)
				mutate(r)
				before := calls
				serve(r, 401)
				if calls != before {
					t.Fatal("invalid proof reached account API")
				}
			}
			r = signed("GET", "/v1/albums/account", "")
			r.Header.Del("Authorization")
			before := calls
			serve(r, 401)
			if calls != before+1 {
				t.Fatal("valid proof did not reach independent account validation")
			}
			serve(signed("POST", "/v1/albums/uploads", `{}`), 403)
			serve(httptest.NewRequest("POST", "/v1/albums/auth/challenges", nil), 401)
			serve(httptest.NewRequest("GET", "/v1/albums/account", nil), 401)
			serve(signed("POST", "/v1/albums/collections", strings.Repeat("x", (2<<20)+1)), 413)
			unknown := signed("GET", "/v1/albums/account", "")
			unknown.Header.Set("X-Tellyouwhat-Key-ID", "unknown-synthetic-key")
			if value := serve(unknown, 401); !strings.Contains(value.Body.String(), `"code":"app_attestation_key_unknown"`) {
				t.Fatal("missing device identity was treated as account rejection")
			}
			// Enrollment exposes neither a developer grant nor the AI product API.
			for _, path := range []string{"/v1/attest/unknown", "/v1/attest/keys/"} {
				serve(httptest.NewRequest("POST", path, nil), 404)
			}
		})
	}
}

func TestAlbumAppAttestPublicExceptionsAreExact(t *testing.T) {
	app := config.AppConfig{Registry: appregistry.App{ID: appregistry.Albums}, AttestationEnvironment: attestation.EnvironmentProduction}
	seen := 0
	h := newAlbumAttestedHandler(app, nil, nil, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen++; w.WriteHeader(204) }), allowAlbumBootstrap)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/readyz", 503}, {"GET", "/livez", 204}, {"POST", "/readyz", 401},
		{"POST", "/v1/albums/auth/apple/notifications", 204}, {"POST", "/v1/albums/subscription/notifications", 204},
		{"POST", "/v1/albums/share-links/lookup", 204}, {"POST", "/v1/albums/share-links/claims", 204},
		{"OPTIONS", "/v1/albums/share-links/read-grants", 204},
		{"GET", "/v1/albums/share-links/lookup", 401}, {"POST", "/v1/albums/share-links/lookup/", 401},
		{"POST", "/v1/albums/auth/apple/notifications/", 401}, {"POST", "/v1/albums/auth/logout", 401},
		{"POST", "/v1/albums/account/deletion", 401}, {"POST", "/v1/dev/entitlements/activate", 401},
	} {
		w := httptest.NewRecorder()
		before := seen
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || (seen != before) != (tc.status == 204) {
			t.Fatalf("%s %s = %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestAlbumAppAttestEnrollmentRejectsForeignIdentityAndSandboxProof(t *testing.T) {
	for _, tc := range []struct{ identity, environment string }{
		{"TEAM.cn.tellyouwhat.healthapp.dev", "production"},
		{"OTHER.cn.tellyouwhat.albums.dev", "production"},
		{"TEAM.cn.tellyouwhat.albums", "production"},
		{"TEAM.cn.tellyouwhat.albums.dev", "development"},
	} {
		t.Run(tc.identity+"-"+tc.environment, func(t *testing.T) {
			client := appattest.New(t, tc.identity, time.Now())
			keys := attestation.NewMemoryKeyStore()
			app := config.AppConfig{Registry: appregistry.App{ID: appregistry.Albums, TeamID: "TEAM", BundleID: "cn.tellyouwhat.albums.dev"}, AttestationEnvironment: attestation.EnvironmentProduction}
			h := newAlbumAttestedHandler(app, attestation.NewMemoryNonceStore(), keys, client.Roots, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid enrollment reached account API") }), allowAlbumBootstrap)
			post := func(path string, body any, status int) *httptest.ResponseRecorder {
				t.Helper()
				data, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("POST", path, bytes.NewReader(data))
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != status {
					t.Fatalf("%s = %d: %s", path, w.Code, w.Body.String())
				}
				return w
			}
			w := post("/v1/attest/challenges", map[string]string{"keyID": client.KeyID}, 201)
			var challenge struct{ Challenge string }
			if json.Unmarshal(w.Body.Bytes(), &challenge) != nil {
				t.Fatal("invalid challenge")
			}
			post("/v1/attest/keys", map[string]string{"keyID": client.KeyID, "challenge": challenge.Challenge,
				"attestation": base64.StdEncoding.EncodeToString(client.Attestation(t, challenge.Challenge, tc.environment)), "build": "9", "activationSecret": ""}, 401)
			if _, err := keys.Get(context.Background(), client.KeyID); err != attestation.ErrKeyNotFound {
				t.Fatal("foreign key persisted")
			}
		})
	}
}

func TestAlbumAppAttestReadinessChecksSharedDependencies(t *testing.T) {
	for _, ready := range []bool{false, true} {
		app := config.AppConfig{Registry: appregistry.App{ID: appregistry.Albums}, AttestationEnvironment: attestation.EnvironmentProduction}
		readiness := gateway.ReadinessFunc(func(context.Context) error {
			if !ready {
				return attestation.ErrUnavailable
			}
			return nil
		})
		h := newAlbumAttestedHandler(app, nil, nil, nil, readiness, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), allowAlbumBootstrap)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		if (ready && w.Code != 204) || (!ready && w.Code != 503) {
			t.Fatalf("ready=%t: status=%d", ready, w.Code)
		}
	}
}

func TestAlbumAppAttestBootstrapUsesRecoveryGuard(t *testing.T) {
	app := config.AppConfig{Registry: appregistry.App{ID: appregistry.Albums}, AttestationEnvironment: attestation.EnvironmentProduction}
	denyBootstrap := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("request unexpectedly reached account API") })
	h := newAlbumAttestedHandler(app, nil, nil, nil, nil, next, denyBootstrap)
	for path, want := range map[string]int{
		"/v1/attest/challenges": http.StatusServiceUnavailable,
		"/v1/attest/keys":       http.StatusServiceUnavailable,
		"/v1/albums/account":    http.StatusUnauthorized,
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code != want {
			t.Errorf("%s = %d, want %d", path, w.Code, want)
		}
	}
}
