package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/tellyouwhat/backend/internal/cloudbilling"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminportal"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/airollout"
	"github.com/tellyouwhat/backend/internal/appstore"
	"github.com/tellyouwhat/backend/internal/appstoreconnect"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	platformconfig "github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/observability"
	"github.com/tellyouwhat/backend/internal/platformops"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"github.com/tellyouwhat/backend/internal/prompteval"
	arkprovider "github.com/tellyouwhat/backend/internal/provider/ark"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("admin stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	configuration, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	database, err := mysqlstore.Open(ctx, configuration.databaseDSN)
	if err != nil {
		return fmt.Errorf("connect admin database: %w", err)
	}
	defer database.Close()
	ops := platformops.Store{DB: database}
	defaults, err := platformconfig.LoadOperationsDefaults()
	if err != nil {
		return err
	}
	if err = ops.Initialize(ctx, defaults, time.Now()); err != nil {
		return err
	}
	redisOptions, err := redis.ParseURL(configuration.redisURL)
	if err != nil {
		return fmt.Errorf("parse admin Redis URL: %w", err)
	}
	redisClient := redis.NewClient(redisOptions)
	defer redisClient.Close()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("connect admin Redis: %w", err)
	}
	repository := adminauth.NewMySQLRepository(database)
	adminAppIDs := make([]string, 0, len(configuration.apps))
	for _, app := range configuration.apps {
		adminAppIDs = append(adminAppIDs, app.id)
	}
	authentication, err := adminauth.NewService(
		repository,
		adminauth.NewRedisStateStore(redisClient),
		adminauth.Config{
			RPID: configuration.rpID, Origin: configuration.origin,
			DisplayName: "Tellyouwhat 管理后台", AppIDs: adminAppIDs,
		},
		time.Now,
	)
	if err != nil {
		return err
	}
	offerClients := make(map[string]adminportal.OfferManager, len(configuration.apps))
	adminApps := make([]adminportal.AdminApp, 0, len(configuration.apps))
	for _, app := range configuration.apps {
		client, err := appstoreconnect.NewClient(appstoreconnect.Config{
			BaseURL: app.baseURL, IssuerID: app.issuerID, KeyID: app.keyID,
			SubscriptionID: app.subscriptionID, SigningKey: app.signingKey,
		})
		if err != nil {
			return fmt.Errorf("configure App Store Connect for %s: %w", app.id, err)
		}
		offerClients[app.id] = client
		adminApps = append(adminApps, adminportal.AdminApp{ID: app.id, DisplayName: app.displayName})
	}
	var bills *cloudbilling.Cache
	var ai *adminportal.AIConfig
	if path := os.Getenv("ARK_MANAGEMENT_CREDENTIAL_FILE"); path != "" {
		billingClient, err := cloudbilling.NewFromFile(path)
		if err != nil {
			return err
		}
		bills = &cloudbilling.Cache{Reader: billingClient}
		bills.Snapshot(time.Now())
		client, err := arkcontrol.NewFromFile(path)
		if err != nil {
			return err
		}
		endpoints := make(map[contracts.Operation]string)
		for _, op := range contracts.OperationValues() {
			endpoints[op] = os.Getenv("HEALTH_ARK_ENDPOINT_" + strings.ToUpper(string(op)))
		}
		timeout := 90
		if raw := os.Getenv("HEALTH_ARK_TIMEOUT_SECONDS"); raw != "" {
			timeout, err = strconv.Atoi(raw)
			if err != nil || timeout < 1 || timeout > 840 {
				return errors.New("invalid health AI timeout")
			}
		}
		shared := make(map[string]bool)
		for _, key := range []string{"JOURNAL_ARK_LITE_MODEL_ID", "JOURNAL_ARK_PRO_MODEL_ID", "JOURNAL_VOICE_MODEL_ID"} {
			if id := os.Getenv(key); id != "" {
				shared[id] = true
			}
		}
		ai = &adminportal.AIConfig{WritesEnabled: strings.EqualFold(os.Getenv("AI_CONFIG_WRITES_ENABLED"), "true"), SharedEndpoints: shared, TimeoutSeconds: timeout, Store: aiconfig.MySQLStore{DB: database}, Inventory: client, Endpoints: endpoints}
		probe := arkprovider.New(arkprovider.Config{BaseURL: "https://ark.cn-beijing.volces.com", APIKey: os.Getenv("HEALTH_ARK_API_KEY")}, nil, nil)
		ai.Rollouts = &airollout.Service{Configurations: ai.Store, Compatibility: &airollout.Compatibility{Probe: probe}, Store: airollout.Store{DB: database}, Cloud: client, Endpoints: endpoints, Shared: shared, WritesEnabled: strings.EqualFold(os.Getenv("AI_ENDPOINT_WRITES_ENABLED"), "true")}

	}
	background, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	promptDefaults, err := platformconfig.LoadPromptDefaults()
	if err != nil {
		return err
	}
	prompts := promptconfig.Store{DB: database}
	promptCache, err := promptconfig.Start(background, prompts, promptDefaults, func(error) { logger.Error("prompt configuration refresh failed; retaining last snapshot") })
	if err != nil {
		return err
	}
	evaluationCipher, err := mysqlstore.NewPayloadCipher(os.Getenv("PAYLOAD_ENCRYPTION_KEY"))
	if err != nil {
		return err
	}
	evaluationCost, err := platformconfig.LoadCostDefaults()
	if err != nil {
		return err
	}
	evaluations := prompteval.Store{DB: database, Cipher: evaluationCipher, Limits: evaluationCost.Limits}
	portal, err := adminportal.NewServer(authentication, offerClients, adminportal.NewMySQLOperationStore(database), adminportal.NewMySQLMetricsReader(database), adminportal.Config{
		AI: ai, Evaluations: &evaluations, EvaluationSpeechPrice: evaluationCost.JournalSpeech,
		Prompts: &prompts, PromptCache: promptCache,
		Billing:                 bills,
		Operations:              &ops,
		OperationsWritesEnabled: strings.EqualFold(os.Getenv("PLATFORM_OPERATIONS_WRITES_ENABLED"), "true"),
		PreviewSigningKey:       configuration.previewSigningKey,
		WritesEnabled:           configuration.writesEnabled,
		Apps:                    adminApps,
		Readiness: func(ctx context.Context) error {
			if err := database.PingContext(ctx); err != nil {
				return err
			}
			return redisClient.Ping(ctx).Err()
		},
		HTTPMiddleware: observability.Middleware(logger),
	}, time.Now)
	if err != nil {
		return err
	}
	if ai != nil {
		go ai.Rollouts.Run(background)
	}
	server := &http.Server{
		Addr: ":" + configuration.port, Handler: portal.Router(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-shutdown
		stopBackground()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	logger.Info("admin service listening", "address", server.Addr)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type config struct {
	port, databaseDSN, redisURL, rpID, origin string
	apps                                      []adminAppConfig
	previewSigningKey                         []byte
	writesEnabled                             bool
}

type adminAppConfig struct {
	id, displayName, baseURL, issuerID, keyID, subscriptionID string
	signingKey                                                *ecdsa.PrivateKey
}

func loadConfig() (config, error) {
	previewSigningKey, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(os.Getenv("ADMIN_PREVIEW_SIGNING_KEY")))
	if err != nil || len(previewSigningKey) < 32 {
		return config{}, errors.New("ADMIN_PREVIEW_SIGNING_KEY must be at least 32 random bytes encoded as unpadded base64")
	}
	configuration := config{
		port: value("ADMIN_PORT", "8082"), databaseDSN: os.Getenv("DATABASE_DSN"), redisURL: os.Getenv("REDIS_URL"),
		rpID:              value("ADMIN_RP_ID", "admin.tellyouwhat.cn"),
		origin:            value("ADMIN_ORIGIN", "https://admin.tellyouwhat.cn"),
		previewSigningKey: previewSigningKey,
		writesEnabled:     strings.EqualFold(os.Getenv("ADMIN_WRITES_ENABLED"), "true"),
	}
	if _, err := mysql.ParseDSN(configuration.databaseDSN); err != nil || configuration.redisURL == "" {
		return config{}, errors.New("admin database and Redis configuration are required")
	}
	for _, definition := range []struct{ prefix, id, name string }{
		{"HEALTH", "health", "告你健康"}, {"JOURNAL", "journal", "告你手记"},
	} {
		app, err := loadAdminApp(definition.prefix, definition.id, definition.name)
		if err != nil {
			return config{}, err
		}
		configuration.apps = append(configuration.apps, app)
	}
	return configuration, nil
}

func loadAdminApp(prefix, id, displayName string) (adminAppConfig, error) {
	read := func(key string) string { return strings.TrimSpace(os.Getenv(prefix + "_" + key)) }
	privateKeyPEM, err := os.ReadFile(read("APP_STORE_CONNECT_PRIVATE_KEY_PATH"))
	if err != nil {
		return adminAppConfig{}, fmt.Errorf("read %s App Store Connect private key: %w", id, err)
	}
	privateKey, err := appstore.ParseSigningKeyPEM(privateKeyPEM)
	if err != nil {
		return adminAppConfig{}, fmt.Errorf("%s App Store Connect key is not a valid P-256 private key", id)
	}
	app := adminAppConfig{
		id: id, displayName: displayName,
		baseURL:  value(prefix+"_APP_STORE_CONNECT_BASE_URL", "https://api.appstoreconnect.apple.com"),
		issuerID: read("APP_STORE_CONNECT_ISSUER_ID"), keyID: read("APP_STORE_CONNECT_KEY_ID"),
		subscriptionID: read("APP_STORE_CONNECT_SUBSCRIPTION_ID"), signingKey: privateKey,
	}
	if app.issuerID == "" || app.keyID == "" || app.subscriptionID == "" {
		return adminAppConfig{}, fmt.Errorf("%s App Store Connect configuration is required", id)
	}
	return app, nil
}

func value(key string, fallback string) string {
	if result := strings.TrimSpace(os.Getenv(key)); result != "" {
		return result
	}
	return fallback
}
