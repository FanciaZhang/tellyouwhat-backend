package main

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminportal"
	"github.com/tellyouwhat/backend/internal/appstore"
	"github.com/tellyouwhat/backend/internal/appstoreconnect"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	configuration, err := loadConfig()
	if err != nil {
		return err
	}
	database, err := sql.Open("mysql", configuration.databaseDSN)
	if err != nil {
		return err
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		return fmt.Errorf("connect admin database: %w", err)
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
	authentication, err := adminauth.NewService(
		repository,
		adminauth.NewRedisStateStore(redisClient),
		adminauth.Config{
			RPID: configuration.rpID, Origin: configuration.origin,
			DisplayName: "Tellyouwhat 管理后台", CookieSecure: configuration.cookieSecure,
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
	portal, err := adminportal.NewServer(authentication, offerClients, adminportal.NewMySQLOperationStore(database), adminportal.NewMySQLMetricsReader(database), adminportal.Config{
		PreviewSigningKey: configuration.previewSigningKey,
		WritesEnabled:     configuration.writesEnabled,
		Apps:              adminApps,
		Readiness: func(ctx context.Context) error {
			if err := database.PingContext(ctx); err != nil {
				return err
			}
			return redisClient.Ping(ctx).Err()
		},
	}, time.Now)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: ":" + configuration.port, Handler: portal,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	log.Printf("admin service listening on %s", server.Addr)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type config struct {
	port, databaseDSN, redisURL, rpID, origin string
	cookieSecure                              bool
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
		cookieSecure:      !strings.EqualFold(os.Getenv("ADMIN_COOKIE_SECURE"), "false"),
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
