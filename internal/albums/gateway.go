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
	db                  *sql.DB
	redis               *redis.Client
	router              http.Handler
	host                string
	draining            atomic.Bool
	closed              sync.Once
	subscriptionCancel  context.CancelFunc
	retentionCancel     context.CancelFunc
	appleDeletionCancel context.CancelFunc
	subscriptionWorkers sync.WaitGroup
}

func NewGateway(ctx context.Context, config GatewayConfig, transport http.RoundTripper) (_ *Gateway, err error) {
	if !validAlbumAudience(config.AppleCode.BundleID) || config.AppleCode.BundleID != albumAudienceForDeployment(config.Environment) ||
		(config.Worker.DevelopmentCloud != nil && (config.Environment != "test" || config.Subscriptions != nil)) ||
		(config.Subscriptions != nil && config.Subscriptions.BundleID != config.AppleCode.BundleID) ||
		(config.RetentionAPNS != nil && config.RetentionAPNS.BundleID != config.AppleCode.BundleID) {
		return nil, errors.New("album runtime identity does not match deployment")
	}
	db, err := mysqlstore.Open(ctx, config.Worker.DatabaseDSN)
	if err != nil {
		return nil, errors.New("album database connection")
	}
	if err := CheckRecoveryState(ctx, db); err != nil {
		db.Close()
		return nil, errors.New("album recovery state unavailable or quarantined")
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
	accounts, err := NewMySQLAccounts(config.AppleCode.BundleID, db, cipher)
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
	verifier := NewAppleIdentityVerifier(config.AppleCode.BundleID, transport)
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
	notificationLimiter, err := NewRedisLoginLimiter(g.redis, config.LoginLimitKey, 1000, time.Minute)
	if err != nil {
		return nil, errors.New("album notification rate limit configuration")
	}
	repository := NewMySQLUploads(db).WithSubscriptionEnvironment(albumStoreEnvironment(config.Environment)).WithDevelopmentCloud(config.Worker.DevelopmentCloud)
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
	var shares *ShareService
	if config.Sharing != nil {
		if err := repository.CheckShareSchema(ctx); err != nil {
			return nil, ErrShareUnavailable
		}
		shares, err = NewShareService(repository, uploads, config.Sharing.SiteOrigin, config.Sharing.Keys, time.Now)
		if err != nil {
			return nil, ErrShareUnavailable
		}
	}
	shareLimiter, err := NewRedisLoginLimiter(g.redis, config.LoginLimitKey, 60, time.Minute)
	if err != nil {
		return nil, ErrShareUnavailable
	}
	shareClaimLimiter, err := NewRedisLoginLimiter(g.redis, config.LoginLimitKey, 10, time.Minute)
	if err != nil {
		return nil, ErrShareUnavailable
	}
	var push *albumPushRegistrations
	if err := config.validatePushEnvironment(); err != nil {
		return nil, err
	}
	if config.PushEnvironment != "" {
		push, err = newAlbumPushRegistrations(db, cipher, config.PushEnvironment)
		if err != nil {
			return nil, errPushRegistration
		}
		push.allowDevelopment = config.PushAllowDevelopment
	}
	pushLimiter, err := NewRedisLoginLimiter(g.redis, config.LoginLimitKey, 60, time.Minute)
	if err != nil {
		return nil, errPushRegistration
	}
	g.router, err = NewHTTPRouterWithLogin(config.APIHost, sessions, uploads, LoginHTTPOptions{
		AppleNotifications: &appleAccountNotifications{verifier, accounts},
		Service:            login, Limiter: limiter, Sessions: sessions, Multipart: multipart, TrustedProxies: config.TrustedProxies,
		Subscriptions:       subscriptions,
		NotificationLimiter: notificationLimiter,
		Shares:              shares, ShareLimiter: shareLimiter, ShareClaimLimiter: shareClaimLimiter,
		PushRegistrations: push, PushLimiter: pushLimiter,
		TrashEnabled: config.TrashEnabled, LibraryLimiter: shareLimiter,
	})
	if err != nil {
		return nil, errors.New("album HTTP configuration")
	}
	var dispatcher *albumRetentionDispatcher
	if config.RetentionAPNS != nil {
		if push == nil || subscriptions == nil || config.RetentionAPNS.Environment != push.environment {
			return nil, errAlbumAPNS
		}
		sender, senderErr := newAlbumAPNS(*config.RetentionAPNS, transport)
		if senderErr != nil {
			return nil, senderErr
		}
		dispatcher = &albumRetentionDispatcher{repository: repository, subscriptions: subscriptions, registrations: push, sender: sender, now: time.Now}
		if push.allowDevelopment {
			development := config.DevelopmentRetentionAPNS
			if development == nil || development.Environment != "development" || development.BundleID != config.AppleCode.BundleID || development.TeamID != config.AppleCode.TeamID {
				return nil, errAlbumAPNS
			}
			dispatcher.developmentSender, err = newAlbumAPNS(*development, transport)
			if err != nil {
				return nil, err
			}
		}
	}
	if err := g.startAppleDeletion(context.WithoutCancel(ctx), accounts, exchanger, config.Worker.BatchSize, config.Worker.PollInterval); err != nil {
		return nil, errors.New("album Apple deletion worker configuration")
	}
	g.startSubscriptionRefresh(context.WithoutCancel(ctx), subscriptions, repository)
	if dispatcher != nil {
		g.startRetentionDispatch(context.WithoutCancel(ctx), dispatcher)
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
		`SELECT subject_sha256,revoked_before,deleted_at FROM album_apple_identity_state LIMIT 0`,
		`SELECT account_id,request_id,requested_at,access_erased_at FROM album_account_deletions LIMIT 0`,
		`SELECT account_id,request_id,authorizing_session_sha256,requested_at,records_erased_at FROM album_deletion_ledger LIMIT 0`,
		`SELECT account_id,request_id,grant_id,available_at,attempts,lease_token,lease_until,acknowledged_at FROM album_deletion_apple_jobs LIMIT 0`,
		`SELECT identity_issued_at FROM album_apple_grants LIMIT 0`,
		`SELECT account_id,apple_subject_sha256,state FROM album_accounts LIMIT 0`,
		`SELECT grant_id,account_id,token_sha256,encrypted_token,token_nonce,revoked_at FROM album_apple_grants LIMIT 0`,
		`SELECT session_id,account_id,grant_id,token_sha256,expires_at,revoked_at FROM album_sessions LIMIT 0`,
		`SELECT registration_id,session_id,environment,state,revision,token_sha256,encrypted_token,token_nonce,registered_at,request_id,request_sha256 FROM album_push_registrations LIMIT 0`,
		`SELECT delivery_id,reminder_id,registration_id,registration_revision,state,available_at,lease_token,accepted_at FROM album_retention_deliveries LIMIT 0`,
		`SELECT challenge_id,proof_sha256,apple_nonce,expires_at,consumed_at FROM album_login_challenges LIMIT 0`,
		`SELECT owner_id,quota_bytes,used_bytes,reserved_bytes,subscription_checked_revision,subscription_checked_at FROM album_storage_accounts LIMIT 0`,
		`SELECT ` + uploadColumns + ` FROM album_uploads LIMIT 0`,
		`SELECT upload_id,resource_id,intent_id,state,provider_upload_id,reconcile_attempts,reconcile_token,reconcile_until,reconcile_after FROM album_multipart_intents LIMIT 0`,
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

func (g *Gateway) BeginDrain() {
	g.draining.Store(true)
	if g.appleDeletionCancel != nil {
		g.appleDeletionCancel()
	}
	if g.retentionCancel != nil {
		g.retentionCancel()
	}
	if g.subscriptionCancel != nil {
		g.subscriptionCancel()
	}
}

func (g *Gateway) Close() {
	g.BeginDrain()
	g.closed.Do(func() { g.subscriptionWorkers.Wait(); _ = g.redis.Close(); _ = g.db.Close() })
}
