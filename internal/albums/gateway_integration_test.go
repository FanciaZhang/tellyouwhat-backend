package albums

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

func gatewayTestDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	raw := os.Getenv("MYSQL_TEST_ADMIN_DSN")
	if raw == "" || os.Getenv("REDIS_TEST_URL") == "" {
		t.Skip("set MYSQL_TEST_ADMIN_DSN and REDIS_TEST_URL for isolated album gateway integration")
	}
	config, err := mysql.ParseDSN(raw)
	if err != nil || !strings.HasSuffix(config.DBName, "_test") {
		t.Fatal("refusing non-test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := mysqlstore.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	name := "albums_gateway" + strings.ReplaceAll(uuid.NewString(), "-", "") + "_test"
	user := "algw_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	password := strings.ReplaceAll(uuid.NewString(), "-", "")
	if !albumTestDatabasePattern.MatchString(name) {
		t.Fatal("invalid generated test schema")
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), "DROP DATABASE `"+name+"`")
		if err != nil {
			t.Error("isolated schema cleanup failed")
		}
		_, err = admin.ExecContext(context.Background(), "DROP USER IF EXISTS '"+user+"'@'%'")
		if err != nil {
			t.Error("isolated user cleanup failed")
		}
		admin.Close()
	})
	if _, err := admin.ExecContext(ctx, "CREATE USER '"+user+"'@'%' IDENTIFIED BY '"+password+"'"); err != nil {
		t.Fatal("test user creation failed")
	}
	if _, err := admin.ExecContext(ctx, "GRANT ALL PRIVILEGES ON `"+name+"`.* TO '"+user+"'@'%'"); err != nil {
		t.Fatal("test schema grant failed")
	}
	config.DBName = name
	config.User, config.Passwd = user, password
	dsn := config.FormatDSN()
	db, err := mysqlstore.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	adminConfig, err := mysql.ParseDSN(raw)
	if err != nil {
		t.Fatal("invalid test administrator DSN")
	}
	connection, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, denied := connection.ExecContext(ctx, "USE `"+strings.ReplaceAll(adminConfig.DBName, "`", "``")+"`")
	connection.Close()
	var permissionError *mysql.MySQLError
	if !errors.As(denied, &permissionError) || permissionError.Number != 1044 {
		t.Fatal("API user was not denied the existing parent schema")
	}
	return db, dsn
}

// Real MySQL, Redis, HTTP sockets, AES-GCM, and RSA/ECDSA signatures. Apple/COS
// responses are controlled; this is not provider, subscription or device proof.
func TestGatewayMySQLRedisAppleLoginAndSessionLifecycle(t *testing.T) {
	db, dsn := gatewayTestDatabase(t)
	values := gatewayEnvironment(t)
	values["ALBUM_DATABASE_DSN"] = dsn
	values["ALBUM_REDIS_URL"] = os.Getenv("REDIS_TEST_URL")
	config, err := LoadGatewayConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	appleKey := testAppleKey(t)
	subject := "gateway-fixture-" + uuid.NewString()
	refresh := "gateway-refresh-fixture-" + uuid.NewString()
	var nonce string
	var codeCalls, cosCalls atomic.Int32
	clientSigningKey, err := jwt.ParseECPrivateKeyFromPEM(config.AppleCode.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	transport := appleTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" {
			t.Error("provider request was not HTTPS")
		}
		if r.URL.Host == config.Worker.COS.Bucket+".cos."+config.Worker.COS.Region+".myqcloud.com" && r.Method == "GET" && r.URL.Query().Has("versioning") {
			cosCalls.Add(1)
			if !strings.Contains(r.Header.Get("Authorization"), "q-ak="+config.Worker.COS.SecretID) {
				t.Error("bucket check was not archive-signed")
			}
			return cosResponse(r, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, ""), nil
		}
		if r.URL.String() == appleIssuer+"/auth/keys" {
			return appleResponse(testAppleJWKS(appleKey, "gateway-fixture-key")), nil
		}
		if r.URL.String() != appleIssuer+"/auth/token" || r.Method != "POST" {
			t.Error("unexpected provider operation")
			return appleResponse(`{"error":"invalid_request"}`), nil
		}
		codeCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error("invalid token request")
		}
		if r.Form.Get("client_id") != albumAppleAudience || r.Form.Get("code") != "one-use-gateway-code" || r.Form.Get("client_secret") == "" {
			t.Error("invalid code exchange wiring")
		}
		token, err := jwt.ParseWithClaims(r.Form.Get("client_secret"), &jwt.RegisteredClaims{}, func(token *jwt.Token) (any, error) {
			if token.Header["kid"] != config.AppleCode.KeyID {
				t.Error("wrong Sign in key ID")
			}
			return &clientSigningKey.PublicKey, nil
		}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(config.AppleCode.TeamID), jwt.WithAudience(appleIssuer), jwt.WithSubject(albumAppleAudience), jwt.WithExpirationRequired())
		if err != nil || !token.Valid {
			t.Error("assembled client secret failed ECDSA/claims verification")
		}
		claims := appleClaims(time.Now())
		claims["sub"] = subject
		claims["nonce"] = nonce
		data, _ := json.Marshal(map[string]string{"id_token": appleSigned(t, appleKey, "gateway-fixture-key", claims), "refresh_token": refresh, "token_type": "Bearer"})
		return appleResponse(string(data)), nil
	})
	if gateway, err := NewGateway(ctx, config, transport); err == nil || gateway != nil || cosCalls.Load() != 0 {
		t.Fatal("missing schema accepted or provider contacted before schema check")
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	gateway, err := NewGateway(ctx, config, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gateway.Close)
	server := httptest.NewServer(gateway)
	t.Cleanup(server.Close)
	client := server.Client()
	request := func(method, path string, body []byte, token, host, forwarded string) (int, []byte, http.Header) {
		t.Helper()
		r, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Host = host
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if err != nil {
			t.Fatal(err)
		}
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Error("response can be cached")
		}
		if bytes.Contains(data, []byte(refresh)) || bytes.Contains(data, []byte(config.Worker.COS.SecretKey)) {
			t.Error("HTTP disclosed provider credentials")
		}
		return response.StatusCode, data, response.Header
	}
	if status, _, _ := request("GET", "/readyz", nil, "", config.APIHost, ""); status != 200 {
		t.Fatal("assembled API not ready")
	}
	if status, _, _ := request("GET", "/readyz", nil, "", "api.health.example", ""); status != 421 {
		t.Fatal("foreign host accepted")
	}
	if status, _, _ := request("GET", "/v1/albums/account", nil, "", config.APIHost, ""); status != 401 {
		t.Fatal("account lacked authentication")
	}
	status, data, _ := request("POST", "/v1/albums/auth/challenges", nil, "", config.APIHost, "")
	var challenge LoginChallenge
	if status != 201 || json.Unmarshal(data, &challenge) != nil || challenge.ID == "" || challenge.Proof == "" {
		t.Fatal("persistent challenge failed")
	}
	nonce = challenge.Nonce
	claims := appleClaims(time.Now())
	claims["sub"] = subject
	claims["nonce"] = nonce
	loginRequest := AppleLoginRequest{ChallengeID: challenge.ID, Proof: challenge.Proof, AuthorizationCode: "one-use-gateway-code", IdentityToken: appleSigned(t, appleKey, "gateway-fixture-key", claims)}
	body, _ := json.Marshal(loginRequest)
	status, data, _ = request("POST", "/v1/albums/auth/apple", body, "", config.APIHost, "")
	var session AlbumSession
	if status != 200 || json.Unmarshal(data, &session) != nil || session.Token == "" || !validOwner(session.AccountID) {
		t.Fatal("assembled Apple login failed", status)
	}
	if status, _, _ := request("POST", "/v1/albums/auth/apple", body, "", config.APIHost, ""); status != 401 || codeCalls.Load() != 1 {
		t.Fatal("one-use challenge/code replayed")
	}
	var quota int64
	if err := db.QueryRowContext(ctx, `SELECT quota_bytes FROM album_storage_accounts WHERE owner_id=?`, session.AccountID).Scan(&quota); err != nil || quota != 0 {
		t.Fatal("login granted unverified cloud capacity")
	}
	var encrypted, storedProof []byte
	if err := db.QueryRowContext(ctx, `SELECT encrypted_token FROM album_apple_grants WHERE account_id=?`, session.AccountID).Scan(&encrypted); err != nil || bytes.Contains(encrypted, []byte(refresh)) {
		t.Fatal("refresh credential not encrypted")
	}
	if err := db.QueryRowContext(ctx, `SELECT proof_sha256 FROM album_login_challenges WHERE challenge_id=?`, challenge.ID).Scan(&storedProof); err != nil || string(storedProof) == challenge.Proof {
		t.Fatal("raw login proof persisted")
	}
	if status, _, _ := request("GET", "/v1/albums/account", nil, session.Token, config.APIHost, ""); status != 200 {
		t.Fatal("new session not usable")
	}
	if status, _, _ := request("GET", "/v1/albums/library/versions", nil, session.Token, config.APIHost, ""); status != 200 {
		t.Fatal("catalogue not wired")
	}
	_, _, _, _, manifest := uploadFixture(t)
	uploadRequest, _ := json.Marshal(map[string]any{"requestID": uuid.NewString(), "manifest": manifest})
	if status, data, _ := request("POST", "/v1/albums/uploads", uploadRequest, session.Token, config.APIHost, ""); status != 403 || !bytes.Contains(data, []byte("storage_quota_exceeded")) {
		t.Fatal("zero-quota account could upload", status)
	}
	restarted, err := NewGateway(ctx, config, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	r := httptest.NewRequest("GET", "http://"+config.APIHost+"/v1/albums/account", nil)
	r.Header.Set("Authorization", "Bearer "+session.Token)
	w := httptest.NewRecorder()
	restarted.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("session lost across gateway restart")
	}
	if status, _, _ := request("POST", "/v1/albums/auth/logout", nil, session.Token, config.APIHost, ""); status != 204 {
		t.Fatal("session revocation failed")
	}
	w = httptest.NewRecorder()
	restarted.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("revoked session accepted by another process")
	}
	for range 9 {
		if status, _, _ := request("POST", "/v1/albums/auth/challenges", nil, "", config.APIHost, ""); status != 201 {
			t.Fatal("unexpected challenge limit", status)
		}
	}
	if status, _, header := request("POST", "/v1/albums/auth/challenges", nil, "", config.APIHost, "198.51.100.25"); status != 429 || header.Get("Retry-After") == "" {
		t.Fatal("spoofed forwarding bypassed Redis limit")
	}
	gateway.BeginDrain()
	if status, _, _ := request("GET", "/readyz", nil, "", config.APIHost, ""); status != 503 {
		t.Fatal("draining process remained ready")
	}
	if status, _, _ := request("GET", "/livez", nil, "", config.APIHost, ""); status != 200 {
		t.Fatal("draining liveness failed")
	}
	if status, _, _ := request("GET", "/v1/albums/account", nil, "", config.APIHost, ""); status != 503 {
		t.Fatal("new work accepted while draining")
	}
	if err := restarted.redis.Close(); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("GET", "http://"+config.APIHost+"/readyz", nil)
	w = httptest.NewRecorder()
	restarted.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("Redis failure left API ready")
	}
	disabled := appleTestTransport(func(r *http.Request) (*http.Response, error) {
		return cosResponse(r, `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`, ""), nil
	})
	if gateway, err := NewGateway(ctx, config, disabled); err == nil || gateway != nil {
		t.Fatal("unversioned bucket accepted")
	}
}
