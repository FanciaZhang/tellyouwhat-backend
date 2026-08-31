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
			DisplayName: "告你健康管理后台", CookieSecure: configuration.cookieSecure,
		},
		time.Now,
	)
	if err != nil {
		return err
	}
	offerClient, err := appstoreconnect.NewClient(appstoreconnect.Config{
		BaseURL: configuration.appStoreBaseURL, IssuerID: configuration.appStoreIssuerID,
		KeyID: configuration.appStoreKeyID, SubscriptionID: configuration.subscriptionID,
		SigningKey: configuration.appStoreKey,
	})
	if err != nil {
		return err
	}
	portal, err := adminportal.NewServer(authentication, offerClient, adminportal.NewMySQLOperationStore(database), adminportal.NewMySQLMetricsReader(database), adminportal.Config{
		PreviewSigningKey: configuration.previewSigningKey,
		WritesEnabled:     configuration.writesEnabled,
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
	appStoreBaseURL, appStoreIssuerID         string
	appStoreKeyID, subscriptionID             string
	appStoreKey                               *ecdsa.PrivateKey
	previewSigningKey                         []byte
	writesEnabled                             bool
}

func loadConfig() (config, error) {
	privateKeyPath := strings.TrimSpace(os.Getenv("APP_STORE_CONNECT_PRIVATE_KEY_PATH"))
	privateKeyPEM, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return config{}, fmt.Errorf("read App Store Connect private key: %w", err)
	}
	privateKey, err := appstore.ParseSigningKeyPEM(privateKeyPEM)
	if err != nil {
		return config{}, errors.New("APP_STORE_CONNECT_PRIVATE_KEY_PATH is not a valid P-256 private key")
	}
	previewSigningKey, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(os.Getenv("ADMIN_PREVIEW_SIGNING_KEY")))
	if err != nil || len(previewSigningKey) < 32 {
		return config{}, errors.New("ADMIN_PREVIEW_SIGNING_KEY must be at least 32 random bytes encoded as unpadded base64")
	}
	configuration := config{
		port: value("ADMIN_PORT", "8082"), databaseDSN: os.Getenv("DATABASE_DSN"), redisURL: os.Getenv("REDIS_URL"),
		rpID:              value("ADMIN_RP_ID", "admin.health.tellyouwhat.cn"),
		origin:            value("ADMIN_ORIGIN", "https://admin.health.tellyouwhat.cn"),
		cookieSecure:      !strings.EqualFold(os.Getenv("ADMIN_COOKIE_SECURE"), "false"),
		appStoreBaseURL:   value("APP_STORE_CONNECT_BASE_URL", "https://api.appstoreconnect.apple.com"),
		appStoreIssuerID:  os.Getenv("APP_STORE_CONNECT_ISSUER_ID"),
		appStoreKeyID:     os.Getenv("APP_STORE_CONNECT_KEY_ID"),
		subscriptionID:    os.Getenv("APP_STORE_CONNECT_SUBSCRIPTION_ID"),
		appStoreKey:       privateKey,
		previewSigningKey: previewSigningKey,
		writesEnabled:     strings.EqualFold(os.Getenv("ADMIN_WRITES_ENABLED"), "true"),
	}
	if _, err := mysql.ParseDSN(configuration.databaseDSN); err != nil || configuration.redisURL == "" ||
		configuration.appStoreIssuerID == "" || configuration.appStoreKeyID == "" || configuration.subscriptionID == "" {
		return config{}, errors.New("admin database, Redis, and App Store Connect configuration are required")
	}
	return configuration, nil
}

func value(key string, fallback string) string {
	if result := strings.TrimSpace(os.Getenv(key)); result != "" {
		return result
	}
	return fallback
}
