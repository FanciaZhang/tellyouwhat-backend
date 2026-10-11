package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

// Gateway owns only album persistence and routes. transport is nil in the
// executable; controlled tests can exercise the real assembly without keys or
// requests reaching Apple/COS. Provider hosts remain fixed in their adapters.
type Gateway struct {
	db       *sql.DB
	redis    *redis.Client
	router   http.Handler
	host     string
	draining atomic.Bool
	closed   sync.Once
}

func NewGateway(ctx context.Context, config GatewayConfig, transport http.RoundTripper) (_ *Gateway, err error) {
	db, err := mysqlstore.Open(ctx, config.Worker.DatabaseDSN)
	if err != nil {
		return nil, errors.New("album database connection")
	}
	options, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		db.Close()
		return nil, errors.New("album Redis configuration")
	}
	options.MaxRetries = -1
	options.DialTimeout, options.ReadTimeout, options.WriteTimeout = 2*time.Second, 2*time.Second, 2*time.Second
	g := &Gateway{db: db, redis: redis.NewClient(options), host: config.APIHost}
	defer func() {
		if err != nil {
			g.Close()
		}
	}()
	if err := g.redis.Ping(ctx).Err(); err != nil {
		return nil, errors.New("album Redis connection")
	}
	if err := g.checkSchema(ctx); err != nil {
		return nil, errors.New("album database schema")
	}
	objects, err := newCOSObjects(config.Worker.COS, transportOrDefault(transport))
	if err != nil {
		return nil, errors.New("album object storage configuration")
	}
	if err := objects.Check(ctx); err != nil {
		return nil, errors.New("album object storage versioning check")
	}
	cipher, err := mysqlstore.NewPayloadCipher(config.CredentialKey)
	if err != nil {
		return nil, errors.New("album credential encryption configuration")
	}
	accounts, err := NewMySQLAccounts(db, cipher)
	if err != nil {
		return nil, errors.New("album account configuration")
	}
	sessions, err := NewMySQLSessions(db, time.Now)
	if err != nil {
		return nil, errors.New("album session configuration")
	}
	challenges, err := NewLoginChallengeService(NewMySQLLoginChallenges(db), time.Now)
	if err != nil {
		return nil, errors.New("album login challenge configuration")
	}
	verifier := NewAppleIdentityVerifier(transport)
	exchanger, err := NewAppleCodeExchanger(config.AppleCode, verifier, transport)
	if err != nil {
		return nil, errors.New("album Apple Sign in configuration")
	}
	login, err := NewLoginService(challenges, exchanger, accounts, sessions)
	if err != nil {
		return nil, errors.New("album login configuration")
	}
	limiter, err := NewRedisLoginLimiter(g.redis, config.LoginLimitKey, 10, time.Minute)
	if err != nil {
		return nil, errors.New("album login rate limit configuration")
	}
	repository := NewMySQLUploads(db).WithSubscriptionEnvironment(albumStoreEnvironment(config.Environment))
	subscriptions, err := newConfiguredAlbumSubscriptions(repository, config.Subscriptions, transport)
	if err != nil {
		return nil, errors.New("album subscription configuration")
	}
	uploads, err := NewUploadService(repository, objects, config.Worker.Policy, time.Now)
	if err != nil {
		return nil, errors.New("album upload configuration")
	}
	multipart, err := NewMultipartService(repository, objects, time.Now)
	if err != nil {
		return nil, errors.New("album multipart configuration")
	}
	g.router, err = NewHTTPRouterWithLogin(config.APIHost, sessions, uploads, LoginHTTPOptions{
		Service: login, Limiter: limiter, Sessions: sessions, Multipart: multipart, TrustedProxies: config.TrustedProxies,
		Subscriptions: subscriptions,
	})
	if err != nil {
		return nil, errors.New("album HTTP configuration")
	}
	return g, nil
}

func transportOrDefault(t http.RoundTripper) http.RoundTripper {
	if t == nil {
		return http.DefaultTransport
	}
	return t
}

func (g *Gateway) checkSchema(ctx context.Context) error {
	// Checking named columns detects an old/incomplete schema before accepting
	// challenges or uploads; this process never applies migrations itself.
	for _, query := range []string{
		`SELECT account_id,apple_subject_sha256,state FROM album_accounts LIMIT 0`,
		`SELECT grant_id,account_id,token_sha256,encrypted_token,token_nonce,revoked_at FROM album_apple_grants LIMIT 0`,
		`SELECT session_id,account_id,grant_id,token_sha256,expires_at,revoked_at FROM album_sessions LIMIT 0`,
		`SELECT challenge_id,proof_sha256,apple_nonce,expires_at,consumed_at FROM album_login_challenges LIMIT 0`,
		`SELECT owner_id,quota_bytes,used_bytes,reserved_bytes FROM album_storage_accounts LIMIT 0`,
		`SELECT ` + uploadColumns + ` FROM album_uploads LIMIT 0`,
		`SELECT upload_id,resource_id,intent_id,state,provider_upload_id FROM album_multipart_intents LIMIT 0`,
		`SELECT upload_id,resource_id,operation_id,state,object_version,reconcile_attempts,reconcile_token,reconcile_until,reconcile_after FROM album_multipart_completions LIMIT 0`,
	} {
		rows, err := g.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	r := NewMySQLUploads(g.db)
	for _, check := range []func(context.Context) error{r.CheckLibrarySchema, r.CheckPhotoSchema, r.CheckVideoSchema,
		r.CheckDerivedSchema, r.CheckLivePairSchema, r.CheckSubscriptionSchema} {
		if err := check(ctx); err != nil {
			return err
		}
	}
	// Multipart handlers need their schema even when no multipart is pending.
	_, err := r.PendingReconciliations(ctx, time.Now().UTC(), 1)
	return err
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	defer func() {
		if recover() != nil {
			writeGatewayStatus(w, http.StatusInternalServerError, "album_service_unavailable")
		}
	}()
	if canonicalHost(r.Host) != g.host {
		writeGatewayStatus(w, http.StatusMisdirectedRequest, "unknown_app_host")
		return
	}
	if r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/readyz" {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if g.draining.Load() || g.db.PingContext(ctx) != nil || g.redis.Ping(ctx).Err() != nil {
				writeGatewayStatus(w, http.StatusServiceUnavailable, "not_ready")
				return
			}
		}
		writeGatewayStatus(w, http.StatusOK, "ok")
		return
	}
	if g.draining.Load() {
		writeGatewayStatus(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	g.router.ServeHTTP(w, r)
}

func writeGatewayStatus(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"code":"` + code + `"}`))
}

func (g *Gateway) BeginDrain() { g.draining.Store(true) }

func (g *Gateway) Close() {
	g.BeginDrain()
	g.closed.Do(func() { _ = g.redis.Close(); _ = g.db.Close() })
}
