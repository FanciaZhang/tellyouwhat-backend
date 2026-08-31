package main

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tellyouwhat/backend/internal/appstore"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/capability"
	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/gateway"
	"github.com/tellyouwhat/backend/internal/jobs"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/observability"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/provider/ark"
	"github.com/tellyouwhat/backend/internal/quota"
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

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	serviceConfig, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	contractManifest, err := contracts.LoadManifest(serviceConfig.SchemaManifestPath)
	if err != nil {
		return err
	}

	rootPEM, err := os.ReadFile(serviceConfig.AppAttestRootPEMPath)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		return errors.New("APP_ATTEST_ROOT_PEM_PATH contains no certificates")
	}

	var nonceStore attestation.NonceStore
	var keys keyRepository
	var entitlementStore entitlementRepository
	var jobStore jobs.Store
	var limiter gateway.Quota
	var quotaReader quota.Reader
	var capabilityUses capability.UseStore
	var mediaRegistry media.Registry
	var usageRecorder usage.Recorder
	var outboxStore jobs.OutboxStore
	var tokenReconciler quota.TokenReconciler
	var readiness gateway.Readiness
	var privacyRepository privacy.Repository
	var privacyCache privacy.CacheCleaner
	var closeStorage func()
	if serviceConfig.StorageMode == "memory" {
		nonceStore = attestation.NewMemoryNonceStore()
		keys = attestation.NewMemoryKeyStore()
		entitlementStore = entitlement.NewMemoryStore()
		jobStore = jobs.NewMemoryStore()
		memoryLimiter := quota.NewMemoryLimiter(serviceConfig.Quota)
		limiter = memoryLimiter
		quotaReader = memoryLimiter
		tokenReconciler = memoryLimiter
		capabilityUses = capability.NewMemoryUseStore()
		mediaRegistry = media.NewMemoryRegistry()
		usageRecorder = usage.NewMemoryRecorder()
		readiness = gateway.ReadinessFunc(func(context.Context) error { return nil })
		privacyRepository = privacy.NewMemoryRepository()
		closeStorage = func() {}
	} else {
		database, openErr := mysqlstore.Open(ctx, serviceConfig.DatabaseDSN)
		if openErr != nil {
			return openErr
		}
		redisClient, openErr := redisstore.Open(ctx, serviceConfig.RedisURL)
		if openErr != nil {
			_ = database.Close()
			return openErr
		}
		payloadCipher, cipherErr := mysqlstore.NewPayloadCipher(serviceConfig.PayloadEncryptionKey)
		if cipherErr != nil {
			_ = database.Close()
			_ = redisClient.Close()
			return cipherErr
		}
		nonceStore = redisstore.NewNonceStore(redisClient)
		keys = mysqlstore.NewKeyRepository(database)
		entitlementStore = mysqlstore.NewEntitlementRepository(database)
		jobRepository := mysqlstore.NewJobRepository(database, payloadCipher)
		jobStore = jobRepository
		outboxStore = jobRepository
		redisLimiter := redisstore.NewQuotaLimiter(redisClient, serviceConfig.Quota)
		limiter = redisLimiter
		quotaReader = redisLimiter
		tokenReconciler = redisLimiter
		capabilityUses = redisstore.NewCapabilityUseStore(redisClient)
		mediaRegistry = mysqlstore.NewMediaRepository(database)
		usageRecorder = mysqlstore.NewUsageRepository(database)
		privacyRepository = mysqlstore.NewPrivacyRepository(database)
		privacyCache = redisstore.NewPrivacyCleaner(redisClient)
		readiness = gateway.ReadinessFunc(func(readyContext context.Context) error {
			if pingErr := database.PingContext(readyContext); pingErr != nil {
				return pingErr
			}
			return redisClient.Ping(readyContext).Err()
		})
		closeStorage = func() {
			_ = database.Close()
			_ = redisClient.Close()
		}
	}
	defer closeStorage()

	attestationVerifier := attestation.NewAppleAttestationVerifier(
		serviceConfig.TeamID,
		serviceConfig.BundleID,
		serviceConfig.AttestationEnvironment,
		roots,
	)
	enrollment := attestation.NewEnrollmentService(attestation.EnrollmentConfig{
		Environment:       serviceConfig.AttestationEnvironment,
		DevelopmentSecret: serviceConfig.DevelopmentSecret,
		AllowedBuilds:     serviceConfig.AllowedBuilds,
	}, nonceStore, keys, attestationVerifier, time.Now)
	authenticator := attestation.NewService(
		nonceStore,
		keys,
		attestation.NewAppleAssertionVerifier(serviceConfig.TeamID, serviceConfig.BundleID),
		time.Now,
	).RequireEnvironment(serviceConfig.AttestationEnvironment)
	entitlementChecker := entitlement.NewChecker(entitlementStore, time.Now)
	var activator gateway.EntitlementActivator
	var productionEntitlement gateway.ProductionEntitlementSync
	var appStoreNotifications gateway.AppStoreNotificationProcessor
	if serviceConfig.Environment == "development" {
		activator = entitlement.NewDevelopmentService(
			entitlementStore,
			serviceConfig.DevelopmentSecret,
			30*24*time.Hour,
			time.Now,
		)
	} else {
		appStoreRootPEM, readErr := os.ReadFile(serviceConfig.AppStore.RootPEMPath)
		if readErr != nil {
			return readErr
		}
		appStoreRoots := x509.NewCertPool()
		if !appStoreRoots.AppendCertsFromPEM(appStoreRootPEM) {
			return errors.New("APP_STORE_ROOT_PEM_PATH contains no certificates")
		}
		privateKeyPEM, readErr := os.ReadFile(serviceConfig.AppStore.PrivateKeyPath)
		if readErr != nil {
			return readErr
		}
		signingKey, parseErr := appstore.ParseSigningKeyPEM(privateKeyPEM)
		if parseErr != nil {
			return parseErr
		}
		appStoreEnvironments := []string{serviceConfig.AppStore.Environment}
		if serviceConfig.AppStore.Environment == "Both" {
			appStoreEnvironments = []string{"Production", "Sandbox"}
		}
		appStoreResolvers := make([]appstore.SubscriptionResolving, 0, len(appStoreEnvironments))
		notificationProcessors := make([]appstore.NotificationProcessing, 0, len(appStoreEnvironments))
		for _, appStoreEnvironment := range appStoreEnvironments {
			appStoreBaseURL := "https://api.storekit.apple.com"
			if appStoreEnvironment == "Sandbox" {
				appStoreBaseURL = "https://api.storekit-sandbox.apple.com"
			}
			appStoreVerifier := appstore.NewTransactionVerifier(appstore.VerifierConfig{
				Roots:       appStoreRoots,
				BundleID:    serviceConfig.BundleID,
				AppAppleID:  serviceConfig.AppStore.AppAppleID,
				Environment: appStoreEnvironment,
				ProductID:   appstore.ManagedSubscriptionProductID,
				Now:         time.Now,
			})
			appStoreAPI := appstore.NewAPIClient(appstore.APIClientConfig{
				BaseURL:     appStoreBaseURL,
				KeyID:       serviceConfig.AppStore.KeyID,
				IssuerID:    serviceConfig.AppStore.IssuerID,
				BundleID:    serviceConfig.BundleID,
				AppAppleID:  serviceConfig.AppStore.AppAppleID,
				Environment: appStoreEnvironment,
				SigningKey:  signingKey,
				HTTPClient:  &http.Client{Timeout: 30 * time.Second},
				Now:         time.Now,
			})
			appStoreResolver := appstore.NewSubscriptionResolver(appStoreVerifier, appStoreAPI, time.Now)
			appStoreResolvers = append(appStoreResolvers, appStoreResolver)
			notificationProcessors = append(
				notificationProcessors,
				appstore.NewNotificationProcessor(appStoreVerifier, appStoreResolver),
			)
		}
		appStoreResolver := appstore.NewMultiEnvironmentSubscriptionResolver(appStoreResolvers...)
		productionEntitlement = entitlement.NewProductionService(
			entitlementStore,
			entitlement.NewAppStoreSubscriptionResolver(appStoreResolver),
			time.Now,
		).WithTransactionBinder(keys)
		notificationProcessor := appstore.NewMultiEnvironmentNotificationProcessor(notificationProcessors...)
		appStoreNotifications = entitlement.NewNotificationService(
			entitlementStore,
			entitlement.NewAppStoreNotificationResolver(notificationProcessor),
		)
	}
	tosStore, err := media.NewTOSStore(serviceConfig.TOS)
	if err != nil {
		return err
	}
	mediaService := media.NewService(tosStore, mediaRegistry, time.Now)
	arkClient := ark.New(serviceConfig.Ark, http.DefaultClient, tosStore)
	jobService := jobs.NewService(jobStore, time.Now)
	jobCapabilities := capability.NewService([]byte(serviceConfig.JobCapabilitySecret), capabilityUses, time.Now)
	var dispatcher jobs.Dispatcher
	if serviceConfig.StorageMode == "memory" {
		dispatcher = jobs.NewLocalDispatcher(jobs.NewWorker(jobStore, arkClient, tokenReconciler))
	} else {
		workerInvoker := jobs.NewHTTPDispatcher(
			serviceConfig.WorkerAsyncURL,
			serviceConfig.WorkerSecret,
			nil,
		)
		pump := jobs.NewOutboxPump(outboxStore, workerInvoker, time.Now)
		go func() {
			if pumpErr := pump.Run(ctx); pumpErr != nil && !errors.Is(pumpErr, context.Canceled) {
				logger.Error("job outbox pump stopped", "error", pumpErr)
			}
		}()
		dispatcher = jobs.DurableQueueDispatcher{}
	}

	dependencies := gateway.Dependencies{
		Authenticator:         authenticator,
		Entitlements:          entitlementChecker,
		Quota:                 limiter,
		QuotaReader:           quotaReader,
		Provider:              arkClient,
		Enrollment:            enrollment,
		Activator:             activator,
		ProductionEntitlement: productionEntitlement,
		AppStoreNotifications: appStoreNotifications,
		Media:                 mediaService,
		Jobs:                  jobService,
		Dispatcher:            dispatcher,
		Capabilities:          jobCapabilities,
		Contracts:             contractManifest,
		Usage:                 usageRecorder,
		Readiness:             readiness,
		Privacy:               privacy.NewService(privacyRepository, tosStore, privacyCache, time.Now),
		ManagedProduct: gateway.ManagedProduct{
			ProductID: "health.ai.subscription.monthly", BillingPeriod: "P1M",
			DailyTokenLimit:   serviceConfig.Quota.DailyTokensPerTransaction,
			MonthlyTokenLimit: serviceConfig.Quota.MonthlyTokensPerTransaction,
			Provider:          "Volcengine Ark", ModelDisclosure: "The server selects a model for each feature and may update it without changing the product.",
			MediaRetention: "up to 24 hours", JobRetention: "up to 24 hours",
			PrivacyURL: "https://health.tellyouwhat.cn/privacy", TermsURL: "https://health.tellyouwhat.cn/terms",
			PrivacyChoicesURL: "https://health.tellyouwhat.cn/privacy-choices", SupportURL: "https://health.tellyouwhat.cn/support",
		},
	}
	if serviceConfig.TrustedIPHeader != "" {
		dependencies.IPResolver = func(request *http.Request) string {
			if value := request.Header.Get(serviceConfig.TrustedIPHeader); value != "" {
				return value
			}
			return request.RemoteAddr
		}
	}
	handler := observability.HTTPLogger(logger, observability.RecoverPanics(logger, gateway.New(dependencies)))
	server := &http.Server{
		Addr:              ":" + serviceConfig.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}

	errChannel := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "port", serviceConfig.Port, "environment", serviceConfig.Environment)
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
