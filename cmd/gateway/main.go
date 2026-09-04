package main

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"

	"github.com/tellyouwhat/backend/internal/appstore"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/capability"
	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/gateway"
	"github.com/tellyouwhat/backend/internal/jobs"
	journalprovider "github.com/tellyouwhat/backend/internal/journal/provider"
	journalservice "github.com/tellyouwhat/backend/internal/journal/service"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/observability"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/provider/ark"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/recognitionquota"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/storage/redisstore"
	"github.com/tellyouwhat/backend/internal/usage"
)

type keyRepository interface {
	attestation.KeyStore
	attestation.EnrollmentKeyStore
	entitlement.TransactionBinder
}

type entitlementRepository interface {
	entitlement.Store
	entitlement.NotificationStore
}

type sharedStorage struct {
	database  *sql.DB
	redis     *redis.Client
	cipher    *mysqlstore.PayloadCipher
	readiness gateway.Readiness
}

type appStorage struct {
	voiceStore                 voice.Store
	nonces                     attestation.NonceStore
	keys                       keyRepository
	entitlements               entitlementRepository
	jobs                       jobs.Store
	outbox                     jobs.OutboxStore
	limiter                    gateway.Quota
	quotaReader                quota.Reader
	freeRecognitionLimiter     gateway.Quota
	freeRecognitionQuotaReader quota.Reader
	recognitionSessions        recognitionquota.Store
	reconciler                 quota.TokenReconciler
	capabilityUses             capability.UseStore
	media                      media.Registry
	usage                      usage.Recorder
	privacy                    privacy.Repository
	privacyCache               privacy.CacheCleaner
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	platform, err := config.LoadPlatform()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rootPEM, err := os.ReadFile(platform.AppAttestRootPEMPath)
	if err != nil {
		return err
	}
	attestationRoots := x509.NewCertPool()
	if !attestationRoots.AppendCertsFromPEM(rootPEM) {
		return errors.New("APP_ATTEST_ROOT_PEM_PATH contains no certificates")
	}
	tosStore, err := media.NewTOSStore(platform.TOS)
	if err != nil {
		return err
	}
	shared, closeStorage, err := openSharedStorage(ctx, platform)
	if err != nil {
		return err
	}
	defer closeStorage()

	registryEntries := make([]appregistry.App, 0, len(platform.Apps))
	routers := make(map[appregistry.AppID]*gin.Engine, len(platform.Apps))
	for _, appConfig := range platform.Apps {
		registryEntries = append(registryEntries, appConfig.Registry)
		storage := storageForApp(platform, shared, appConfig)
		appReadiness := shared.readiness
		if platform.StorageMode == "mysql" && appConfig.Registry.ID == appregistry.Health {
			appReadiness, err = readinessWithWorker(shared.readiness, platform.WorkerAsyncURL, nil)
			if err != nil {
				return err
			}
		}
		router, buildErr := buildAppHandler(ctx, platform, appConfig, storage, appReadiness, attestationRoots, tosStore, logger)
		if buildErr != nil {
			return fmt.Errorf("build app %s: %w", appConfig.Registry.ID, buildErr)
		}
		routers[appConfig.Registry.ID] = router
	}
	registry, err := appregistry.New(registryEntries)
	if err != nil {
		return err
	}
	hostMux, err := appregistry.NewHostMux(registry, routers)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              ":" + platform.Port,
		Handler:           hostMux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	errChannel := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "port", platform.Port, "environment", platform.Environment, "apps", len(platform.Apps))
		errChannel <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	case err := <-errChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func readinessWithWorker(base gateway.Readiness, asyncURL string, client *http.Client) (gateway.Readiness, error) {
	workerURL, err := url.Parse(asyncURL)
	if err != nil || (workerURL.Scheme != "http" && workerURL.Scheme != "https") || workerURL.Host == "" {
		return nil, errors.New("WORKER_ASYNC_URL must be an absolute HTTP URL")
	}
	workerURL.Path = "/healthz"
	workerURL.RawPath = ""
	workerURL.RawQuery = ""
	workerURL.Fragment = ""
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	return gateway.ReadinessFunc(func(ctx context.Context) error {
		if base == nil {
			return errors.New("shared storage readiness is unavailable")
		}
		if err := base.Ready(ctx); err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, workerURL.String(), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("worker readiness: %w", err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("worker readiness status %d", response.StatusCode)
		}
		return nil
	}), nil
}

func openSharedStorage(ctx context.Context, platform config.PlatformConfig) (sharedStorage, func(), error) {
	if platform.StorageMode == "memory" {
		return sharedStorage{readiness: gateway.ReadinessFunc(func(context.Context) error { return nil })}, func() {}, nil
	}
	database, err := mysqlstore.Open(ctx, platform.DatabaseDSN)
	if err != nil {
		return sharedStorage{}, nil, err
	}
	redisClient, err := redisstore.Open(ctx, platform.RedisURL)
	if err != nil {
		_ = database.Close()
		return sharedStorage{}, nil, err
	}
	cipher, err := mysqlstore.NewPayloadCipher(platform.PayloadEncryptionKey)
	if err != nil {
		_ = database.Close()
		_ = redisClient.Close()
		return sharedStorage{}, nil, err
	}
	readiness := gateway.ReadinessFunc(func(readyContext context.Context) error {
		if err := database.PingContext(readyContext); err != nil {
			return err
		}
		return redisClient.Ping(readyContext).Err()
	})
	return sharedStorage{database: database, redis: redisClient, cipher: cipher, readiness: readiness}, func() {
		_ = database.Close()
		_ = redisClient.Close()
	}, nil
}

func storageForApp(platform config.PlatformConfig, shared sharedStorage, appConfig config.AppConfig) appStorage {
	appID := string(appConfig.Registry.ID)
	limits := appConfig.Quota
	if platform.StorageMode == "memory" {
		limiter := quota.NewMemoryLimiter(limits)
		storage := appStorage{
			nonces: attestation.NewMemoryNonceStore(), keys: attestation.NewMemoryKeyStore(),
			entitlements: entitlement.NewMemoryStore(), jobs: jobs.NewMemoryStore(),
			limiter: limiter, quotaReader: limiter, reconciler: limiter,
			capabilityUses: capability.NewMemoryUseStore(), media: media.NewMemoryRegistry(),
			usage: usage.NewMemoryRecorder(), privacy: privacy.NewMemoryRepository(), voiceStore: voice.NewMemoryStore(),
		}
		if appConfig.Registry.ID == appregistry.Health {
			freeLimiter := quota.NewMemoryLimiter(appConfig.FreeRecognitionQuota)
			storage.freeRecognitionLimiter = freeLimiter
			storage.freeRecognitionQuotaReader = freeLimiter
			storage.recognitionSessions = recognitionquota.NewMemoryStore()
			storage.reconciler = quota.NewRoutedTokenReconciler(limiter, freeLimiter)
		}
		return storage
	}
	limiter := redisstore.NewQuotaLimiter(shared.redis, limits, appID)
	jobRepository := mysqlstore.NewJobRepository(shared.database, shared.cipher, appID)
	storage := appStorage{
		nonces: redisstore.NewNonceStore(shared.redis, appID), keys: mysqlstore.NewKeyRepository(shared.database, appID),
		entitlements: mysqlstore.NewEntitlementRepository(shared.database, appID), jobs: jobRepository,
		outbox: jobRepository, limiter: limiter, quotaReader: limiter, reconciler: limiter,
		capabilityUses: redisstore.NewCapabilityUseStore(shared.redis, appID),
		media:          mysqlstore.NewMediaRepository(shared.database, appID), usage: mysqlstore.NewUsageRepository(shared.database, appID),
		privacy: mysqlstore.NewPrivacyRepository(shared.database, appID), privacyCache: redisstore.NewPrivacyCleaner(shared.redis, appID),
	}
	if appConfig.Registry.ID == appregistry.Health {
		freeLimiter := redisstore.NewQuotaLimiter(shared.redis, appConfig.FreeRecognitionQuota, appID)
		storage.freeRecognitionLimiter = freeLimiter
		storage.freeRecognitionQuotaReader = freeLimiter
		storage.recognitionSessions = redisstore.NewRecognitionQuotaStore(shared.redis, appID)
		storage.reconciler = quota.NewRoutedTokenReconciler(limiter, freeLimiter)
	}
	if shared.redis != nil {
		storage.voiceStore = voice.RedisStore{Client: shared.redis, Cipher: shared.cipher}
	} else {
		storage.voiceStore = voice.NewMemoryStore()
	}
	return storage
}

func buildAppHandler(
	ctx context.Context,
	platform config.PlatformConfig,
	appConfig config.AppConfig,
	storage appStorage,
	readiness gateway.Readiness,
	attestationRoots *x509.CertPool,
	tosStore *media.TOSStore,
	logger *slog.Logger,
) (*gin.Engine, error) {
	app := appConfig.Registry
	attestationVerifier := attestation.NewAppleAttestationVerifier(app.TeamID, app.BundleID, appConfig.AttestationEnvironment, attestationRoots)
	enrollment := attestation.NewEnrollmentService(attestation.EnrollmentConfig{
		AppID: string(app.ID), Environment: appConfig.AttestationEnvironment,
		DevelopmentSecret: appConfig.DevelopmentSecret, AllowedBuilds: appConfig.AllowedBuilds,
	}, storage.nonces, storage.keys, attestationVerifier, time.Now)
	authenticator := attestation.NewService(
		storage.nonces, storage.keys, attestation.NewAppleAssertionVerifier(app.TeamID, app.BundleID), time.Now,
	).RequireEnvironment(appConfig.AttestationEnvironment)
	entitlementChecker := entitlement.NewChecker(storage.entitlements, time.Now)
	activator, productionSync, notifications, err := commerceServices(platform.Environment, appConfig, storage.keys, storage.entitlements)
	if err != nil {
		return nil, err
	}
	privacyService := privacy.NewService(storage.privacy, tosStore, storage.privacyCache, time.Now)
	mediaService := media.NewService(tosStore, storage.media, time.Now)

	dependencies := gateway.Dependencies{
		App: app, Authenticator: authenticator, Entitlements: entitlementChecker,
		Quota: storage.limiter, QuotaReader: storage.quotaReader,
		Enrollment: enrollment, Activator: activator, ProductionEntitlement: productionSync,
		AppStoreNotifications: notifications, Usage: storage.usage, Readiness: readiness,
		Privacy: privacyService, Consent: privacyService, Media: mediaService,
		ManagedProduct: gateway.ManagedProduct{
			ProductID: app.ManagedAIProductID, BillingPeriod: appConfig.Product.BillingPeriod,
			DailyTokenLimit: appConfig.Quota.DailyTokensPerTransaction, MonthlyTokenLimit: appConfig.Quota.MonthlyTokensPerTransaction,
			Provider: "Volcengine Ark", ModelDisclosure: "The server selects a model for each fixed application operation.",
			MediaRetention: "up to 24 hours", JobRetention: "up to 24 hours",
			PrivacyURL: appConfig.Product.PrivacyURL, TermsURL: appConfig.Product.TermsURL,
			PrivacyChoicesURL: appConfig.Product.PrivacyChoicesURL, SupportURL: appConfig.Product.SupportURL,
		},
		HTTPMiddleware: observability.MiddlewareForApp(logger, string(app.ID)),
	}
	if platform.TrustedIPHeader != "" {
		dependencies.IPResolver = func(request *http.Request) string {
			if value := request.Header.Get(platform.TrustedIPHeader); value != "" {
				return value
			}
			return request.RemoteAddr
		}
	}

	switch app.ID {
	case appregistry.Health:
		dependencies.AllowedConsentScopes = []string{
			privacy.AdultScope, privacy.PrivacyTermsScope, privacy.LifetimeBYOKScope,
			privacy.ManagedAIScope, privacy.FreeRecognitionScope, privacy.SensitiveHealthScope,
		}
		manifest, err := contracts.LoadManifest(appConfig.SchemaManifestPath)
		if err != nil {
			return nil, err
		}
		provider := ark.New(appConfig.Ark, http.DefaultClient, tosStore)
		jobService := jobs.NewService(storage.jobs, time.Now)
		capabilities := capability.NewService([]byte(platform.JobCapabilitySecret), storage.capabilityUses, time.Now)
		var dispatcher jobs.Dispatcher
		if platform.StorageMode == "memory" {
			dispatcher = jobs.NewLocalDispatcher(jobs.NewWorker(storage.jobs, provider, storage.reconciler))
		} else {
			workerInvoker := jobs.NewHTTPDispatcher(platform.WorkerAsyncURL, platform.WorkerSecret, string(app.ID), nil)
			pump := jobs.NewOutboxPump(storage.outbox, workerInvoker, time.Now)
			go func() {
				if err := pump.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Error("job outbox pump stopped", "app_id", app.ID, "error", err)
				}
			}()
			dispatcher = jobs.DurableQueueDispatcher{}
		}
		dependencies.Provider = provider
		dependencies.FreeRecognitionQuota = storage.freeRecognitionLimiter
		dependencies.FreeRecognitionQuotaReader = storage.freeRecognitionQuotaReader
		dependencies.RecognitionSessions = storage.recognitionSessions
		dependencies.Jobs = jobService
		dependencies.Dispatcher = dispatcher
		dependencies.Capabilities = capabilities
		dependencies.Contracts = manifest
		dependencies.RequiredConsentScopes = []string{privacy.SensitiveHealthScope}
	case appregistry.Journal:
		dependencies.AllowedConsentScopes = []string{privacy.ManagedAIScope}
		model := journalprovider.New(journalprovider.Config{
			BaseURL: appConfig.JournalAI.BaseURL, APIKey: appConfig.JournalAI.APIKey,
			LiteModel: appConfig.JournalAI.LiteModel, ProModel: appConfig.JournalAI.ProModel,
		}, &http.Client{Timeout: time.Duration(appConfig.JournalAI.TimeoutSeconds) * time.Second})
		organizer := &journalservice.Organizer{
			Model: model, LiteMaxCharacters: 6_000, LiteMaxBooks: 24, LiteMaxTags: 80,
			AnalysisVersion: "journal-organize-2026-08-31",
		}
		if appConfig.VoiceEnabled {
			if (appConfig.VoiceASR.APIKey == "" && (appConfig.VoiceASR.AppKey == "" || appConfig.VoiceASR.AccessKey == "")) || appConfig.VoiceModel == "" || len(platform.JobCapabilitySecret) < 32 {
				return nil, errors.New("voice requires speech credentials, model, and a 32-byte capability secret")
			}
			dependencies.Voice = &voice.Service{Store: storage.voiceStore, Speech: voice.ASR{Config: appConfig.VoiceASR}, Model: voice.ArkRewriter{BaseURL: appConfig.JournalAI.BaseURL, APIKey: appConfig.JournalAI.APIKey, Model: appConfig.VoiceModel}, Secret: []byte(platform.JobCapabilitySecret), Usage: func(ctx context.Context, identity voice.Identity, input, output int) {
				if err := storage.usage.Record(ctx, usage.Record{RequestID: uuid.NewString(), KeyID: identity.KeyID, Operation: contracts.Operation("journal.voice"), InputTokens: input, OutputTokens: output, OccurredAt: time.Now()}); err != nil {
					logger.Error("voice usage record failed")
				}
			}}
			dependencies.VoiceEntitlements = storage.entitlements
		}
		dependencies.JournalOrganizer = organizer
		dependencies.JournalAnalysisVersion = organizer.AnalysisVersion
		dependencies.RequiredConsentScopes = []string{privacy.ManagedAIScope}
	default:
		return nil, errors.New("unsupported app runtime")
	}
	return gateway.New(dependencies).Router(), nil
}

func commerceServices(
	environment string,
	appConfig config.AppConfig,
	keys keyRepository,
	store entitlementRepository,
) (gateway.EntitlementActivator, gateway.ProductionEntitlementSync, gateway.AppStoreNotificationProcessor, error) {
	if environment == "development" {
		return entitlement.NewDevelopmentService(store, appConfig.DevelopmentSecret, 30*24*time.Hour, time.Now), nil, nil, nil
	}
	rootPEM, err := os.ReadFile(appConfig.AppStore.RootPEMPath)
	if err != nil {
		return nil, nil, nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		return nil, nil, nil, errors.New("App Store root PEM contains no certificates")
	}
	privateKeyPEM, err := os.ReadFile(appConfig.AppStore.PrivateKeyPath)
	if err != nil {
		return nil, nil, nil, err
	}
	signingKey, err := appstore.ParseSigningKeyPEM(privateKeyPEM)
	if err != nil {
		return nil, nil, nil, err
	}
	environments := []string{appConfig.AppStore.Environment}
	if appConfig.AppStore.Environment == "Both" {
		environments = []string{"Production", "Sandbox"}
	}
	resolvers := make([]appstore.SubscriptionResolving, 0, len(environments))
	processors := make([]appstore.NotificationProcessing, 0, len(environments))
	for _, appStoreEnvironment := range environments {
		baseURL := "https://api.storekit.apple.com"
		if appStoreEnvironment == "Sandbox" {
			baseURL = "https://api.storekit-sandbox.apple.com"
		}
		verifier := appstore.NewTransactionVerifier(appstore.VerifierConfig{
			Roots: roots, BundleID: appConfig.Registry.BundleID, AppAppleID: appConfig.Registry.AppAppleID,
			Environment: appStoreEnvironment, ProductIDs: appConfig.ManagedAIProductIDs, Now: time.Now,
		})
		api := appstore.NewAPIClient(appstore.APIClientConfig{
			BaseURL: baseURL, KeyID: appConfig.AppStore.KeyID, IssuerID: appConfig.AppStore.IssuerID,
			BundleID: appConfig.Registry.BundleID, AppAppleID: appConfig.Registry.AppAppleID,
			Environment: appStoreEnvironment, SigningKey: signingKey,
			HTTPClient: &http.Client{Timeout: 30 * time.Second}, Now: time.Now,
		})
		resolver := appstore.NewSubscriptionResolver(verifier, api, time.Now)
		resolvers = append(resolvers, resolver)
		processors = append(processors, appstore.NewNotificationProcessor(verifier, resolver))
	}
	resolver := appstore.NewMultiEnvironmentSubscriptionResolver(resolvers...)
	production := entitlement.NewProductionService(store, entitlement.NewAppStoreSubscriptionResolver(resolver), time.Now).WithTransactionBinder(keys)
	notifications := entitlement.NewNotificationService(
		store, entitlement.NewAppStoreNotificationResolver(appstore.NewMultiEnvironmentNotificationProcessor(processors...)),
	)
	return nil, production, notifications, nil
}
