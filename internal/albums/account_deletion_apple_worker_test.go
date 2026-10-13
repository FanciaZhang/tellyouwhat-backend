package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

type appleGatewayFixture struct {
	db       *sql.DB
	config   GatewayConfig
	accounts *MySQLAccounts
	sessions *MySQLSessions
}

func newAppleGatewayFixture(t *testing.T) appleGatewayFixture {
	t.Helper()
	db, dsn := gatewayTestDatabase(t)
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	env := gatewayEnvironment(t)
	env["ALBUM_DATABASE_DSN"] = dsn
	env["ALBUM_REDIS_URL"] = os.Getenv("REDIS_TEST_URL")
	config, err := LoadGatewayConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	config.Worker.PollInterval = time.Second
	cipher, err := mysqlstore.NewPayloadCipher(config.CredentialKey)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := NewMySQLAccounts(db, cipher)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := NewMySQLSessions(db, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return appleGatewayFixture{db, config, accounts, sessions}
}
func (f appleGatewayFixture) login(t *testing.T, subject string) (AlbumSession, AccountLogin, string) {
	t.Helper()
	token := "synthetic-refresh-" + uuid.NewString()
	grant, err := f.accounts.RegisterAppleLogin(context.Background(), AppleGrant{Identity: AppleIdentity{Subject: subject}, RefreshToken: token})
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.sessions.Issue(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	return session, grant, token
}
func (f appleGatewayFixture) delete(t *testing.T, session AlbumSession) {
	t.Helper()
	if _, err := f.accounts.requestAccountDeletion(context.Background(), session.Token, uuid.NewString(), time.Now); err != nil {
		t.Fatal(err)
	}
}
func (f appleGatewayFixture) transport(t *testing.T, revoke func(*http.Request) (*http.Response, error)) http.RoundTripper {
	t.Helper()
	key, err := jwt.ParseECPrivateKeyFromPEM(f.config.AppleCode.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return appleTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == f.config.Worker.COS.Bucket+".cos."+f.config.Worker.COS.Region+".myqcloud.com" && req.URL.Query().Has("versioning") && req.Method == "GET" {
			return cosResponse(req, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, ""), nil
		}
		if req.URL.String() != appleIssuer+"/auth/revoke" || req.Method != "POST" {
			t.Error("unexpected provider route")
			return nil, errors.New("unexpected route")
		}
		if err := req.ParseForm(); err != nil {
			return nil, err
		}
		if req.Form.Get("client_id") != albumAppleAudience || req.Form.Get("token_type_hint") != "refresh_token" || len(req.Form) != 4 {
			t.Error("wrong app revocation form")
			return nil, errors.New("wrong form")
		}
		claims := jwt.RegisteredClaims{}
		_, err := jwt.ParseWithClaims(req.Form.Get("client_secret"), &claims, func(token *jwt.Token) (any, error) {
			if token.Header["kid"] != f.config.AppleCode.KeyID {
				return nil, errors.New("wrong signing key")
			}
			return &key.PublicKey, nil
		}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(f.config.AppleCode.TeamID), jwt.WithAudience(appleIssuer), jwt.WithSubject(albumAppleAudience), jwt.WithExpirationRequired())
		if err != nil {
			t.Error("invalid revocation signature")
			return nil, err
		}
		return revoke(req)
	})
}
func waitAppleGateway(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Apple deletion runtime did not reach expected state")
}
func TestGatewayAppleDeletionRevokesOnlyAcceptedAccountAndWipesAcknowledgedCredentials(t *testing.T) {
	f := newAppleGatewayFixture(t)
	session, first, token1 := f.login(t, "deleted")
	_, second, token2 := f.login(t, "deleted")
	other, keep, _ := f.login(t, "retained")
	f.delete(t, session)
	var calls atomic.Int32
	transport := f.transport(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		token := req.Form.Get("token")
		if token != token1 && token != token2 {
			t.Error("revoked unrelated credential")
		}
		return appleResponse(""), nil
	})
	gateway, err := NewGateway(context.Background(), f.config, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	waitAppleGateway(t, func() bool {
		var n int
		err := f.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_apple_jobs WHERE acknowledged_at IS NOT NULL`).Scan(&n)
		return err == nil && n == 2
	})
	if calls.Load() != 2 {
		t.Fatal("unexpected revocation count", calls.Load())
	}
	for _, grant := range []AccountLogin{first, second} {
		if _, err := f.accounts.RefreshCredential(context.Background(), grant.AccountID, grant.GrantID); err == nil {
			t.Fatal("acknowledged credential retained")
		}
	}
	if _, err := f.accounts.RefreshCredential(context.Background(), keep.AccountID, keep.GrantID); err != nil {
		t.Fatal("other credential removed", err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://api.albums.example/v1/albums", nil)
	request.Header.Set("Authorization", "Bearer "+other.Token)
	if _, err := f.sessions.Authenticate(context.Background(), request); err != nil {
		t.Fatal("other session stopped", err)
	}
	var state string
	if err = f.db.QueryRow(`SELECT state FROM album_accounts WHERE account_id=?`, session.AccountID).Scan(&state); err != nil || state != "deleting" {
		t.Fatal("revocation declared account erasure", state, err)
	}
	gateway.Close()
	restarted, err := NewGateway(context.Background(), f.config, transport)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	if calls.Load() != 2 {
		t.Fatal("acknowledged grants repeated")
	}
}
func TestGatewayAppleDeletionFailedProviderPersistsBackoffAcrossRestart(t *testing.T) {
	f := newAppleGatewayFixture(t)
	session, grant, _ := f.login(t, "retry")
	f.delete(t, session)
	var calls atomic.Int32
	var success atomic.Bool
	transport := f.transport(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		if success.Load() {
			return appleResponse(""), nil
		}
		response := appleResponse(`{"error":"unavailable"}`)
		response.StatusCode = 503
		return response, nil
	})
	gateway, err := NewGateway(context.Background(), f.config, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gateway.Close)
	waitAppleGateway(t, func() bool {
		var n int
		err := f.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_apple_jobs WHERE attempts=1 AND lease_token IS NULL AND acknowledged_at IS NULL AND available_at>?`, time.Now().UTC()).Scan(&n)
		return err == nil && n == 1
	})
	gateway.Close()
	if _, err = f.accounts.RefreshCredential(context.Background(), grant.AccountID, grant.GrantID); err != nil {
		t.Fatal("failed revoke lost credential", err)
	}
	success.Store(true)
	restarted, err := NewGateway(context.Background(), f.config, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	time.Sleep(1100 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("restart ignored persisted backoff")
	}
	if _, err = f.db.Exec(`UPDATE album_deletion_apple_jobs SET available_at=? WHERE grant_id=?`, time.Now().UTC().Add(-time.Second), grant.GrantID); err != nil {
		t.Fatal(err)
	}
	waitAppleGateway(t, func() bool {
		var n int
		err := f.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_apple_jobs WHERE acknowledged_at IS NOT NULL AND attempts=2`).Scan(&n)
		return err == nil && n == 1
	})
	if calls.Load() != 2 {
		t.Fatal("retry count", calls.Load())
	}
}
func TestGatewayAppleDeletionDrainWaitsForTransportAndRetainsUncertainCredential(t *testing.T) {
	f := newAppleGatewayFixture(t)
	session, grant, _ := f.login(t, "drain")
	f.delete(t, session)
	began, release := make(chan struct{}), make(chan struct{})
	transport := f.transport(t, func(req *http.Request) (*http.Response, error) {
		close(began)
		<-req.Context().Done()
		<-release
		return nil, req.Context().Err()
	})
	gateway, err := NewGateway(context.Background(), f.config, transport)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-began:
	case <-time.After(5 * time.Second):
		close(release)
		gateway.Close()
		t.Fatal("revocation not started")
	}
	done := make(chan struct{})
	go func() { gateway.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("closed persistence before transport exit")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not close")
	}
	if _, err = f.accounts.RefreshCredential(context.Background(), grant.AccountID, grant.GrantID); err != nil {
		t.Fatal("uncertain credential removed", err)
	}
	var ack sql.NullTime
	var lease sql.NullString
	if err = f.db.QueryRow(`SELECT acknowledged_at,lease_token FROM album_deletion_apple_jobs WHERE grant_id=?`, grant.GrantID).Scan(&ack, &lease); err != nil || ack.Valid || !lease.Valid {
		t.Fatal("lost recovery lease", err)
	}
}
func TestGatewayAppleDeletionRequiresQueueSchemaAndValidScheduling(t *testing.T) {
	f := newAppleGatewayFixture(t)
	calls := 0
	transport := f.transport(t, func(*http.Request) (*http.Response, error) { calls++; return appleResponse(""), nil })
	if _, err := f.db.Exec(`ALTER TABLE album_deletion_apple_jobs RENAME COLUMN available_at TO missing_available_at`); err != nil {
		t.Fatal(err)
	}
	defer f.db.Exec(`ALTER TABLE album_deletion_apple_jobs RENAME COLUMN missing_available_at TO available_at`)
	if gateway, err := NewGateway(context.Background(), f.config, transport); err == nil {
		gateway.Close()
		t.Fatal("missing queue schema accepted")
	}
	if calls != 0 {
		t.Fatal("provider contacted before schema check")
	}
	if err := (&Gateway{}).startAppleDeletion(f.accounts, nil, 1, time.Second); !errors.Is(err, ErrInvalidManifest) {
		t.Fatal(err)
	}
}
