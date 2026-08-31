package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/provider/ark"
	"github.com/tellyouwhat/backend/internal/quota"
)

type Config struct {
	Environment            string
	Port                   string
	StorageMode            string
	DatabaseDSN            string
	RedisURL               string
	PayloadEncryptionKey   string
	TeamID                 string
	BundleID               string
	AttestationEnvironment attestation.Environment
	AppAttestRootPEMPath   string
	DevelopmentSecret      string
	AllowedBuilds          map[string]struct{}
	WorkerSecret           string
	WorkerAsyncURL         string
	JobCapabilitySecret    string
	SchemaManifestPath     string
	TrustedIPHeader        string
	Ark                    ark.Config
	TOS                    media.TOSConfig
	Quota                  quota.Limits
	AppStore               AppStoreConfig
}

type AppStoreConfig struct {
	Environment    string
	IssuerID       string
	KeyID          string
	PrivateKeyPath string
	RootPEMPath    string
	AppAppleID     int64
}

func Load() (Config, error) {
	config, err := loadUnchecked()
	if err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func LoadWorker() (Config, error) {
	config, err := loadUnchecked()
	if err != nil {
		return Config{}, err
	}
	if config.StorageMode != "mysql" || config.DatabaseDSN == "" || config.RedisURL == "" || config.PayloadEncryptionKey == "" {
		return Config{}, errors.New("worker requires mysql, redis, and PAYLOAD_ENCRYPTION_KEY")
	}
	if config.WorkerSecret == "" {
		return Config{}, errors.New("WORKER_INTERNAL_SECRET is required")
	}
	if err := validateArkConfig(config.Ark); err != nil {
		return Config{}, err
	}
	if config.TOS.Bucket == "" || config.TOS.AccessKey == "" || config.TOS.SecretKey == "" {
		return Config{}, errors.New("TOS bucket credentials are required")
	}
	return config, nil
}

func loadUnchecked() (Config, error) {
	environment := value("APP_ENV", "development")
	attestationEnvironment := attestation.Environment(value("APP_ATTEST_ENV", environment))
	arkTimeout, err := parsedInt("ARK_TIMEOUT_SECONDS", 90)
	if err != nil {
		return Config{}, err
	}
	quotaIP, err := parsedInt("QUOTA_REQUESTS_PER_MINUTE_IP", 30)
	if err != nil {
		return Config{}, err
	}
	quotaDevice, err := parsedInt("QUOTA_REQUESTS_PER_MINUTE_DEVICE", 20)
	if err != nil {
		return Config{}, err
	}
	quotaOperation, err := parsedInt("QUOTA_REQUESTS_PER_MINUTE_OPERATION", 10)
	if err != nil {
		return Config{}, err
	}
	quotaDaily, err := parsedInt("QUOTA_DAILY_TOKENS", 300_000)
	if err != nil {
		return Config{}, err
	}
	quotaMonthly, err := parsedInt("QUOTA_MONTHLY_TOKENS", 5_000_000)
	if err != nil {
		return Config{}, err
	}
	quotaConcurrent, err := parsedInt("QUOTA_MAX_CONCURRENT_DEVICE", 2)
	if err != nil {
		return Config{}, err
	}
	appAppleID, err := parsedInt64Optional("APP_STORE_APP_APPLE_ID")
	if err != nil {
		return Config{}, err
	}
	config := Config{
		Environment:            environment,
		Port:                   value("PORT", "8080"),
		StorageMode:            value("STORAGE_MODE", "mysql"),
		DatabaseDSN:            os.Getenv("DATABASE_DSN"),
		RedisURL:               os.Getenv("REDIS_URL"),
		PayloadEncryptionKey:   os.Getenv("PAYLOAD_ENCRYPTION_KEY"),
		TeamID:                 os.Getenv("APPLE_TEAM_ID"),
		BundleID:               value("APPLE_BUNDLE_ID", "cn.tellyouwhat.healthapp"),
		AttestationEnvironment: attestationEnvironment,
		AppAttestRootPEMPath:   os.Getenv("APP_ATTEST_ROOT_PEM_PATH"),
		DevelopmentSecret:      os.Getenv("DEV_ACTIVATION_SECRET"),
		AllowedBuilds:          splitSet(os.Getenv("ALLOWED_APP_BUILDS")),
		WorkerSecret:           os.Getenv("WORKER_INTERNAL_SECRET"),
		WorkerAsyncURL:         os.Getenv("WORKER_ASYNC_URL"),
		JobCapabilitySecret:    os.Getenv("JOB_CAPABILITY_SECRET"),
		SchemaManifestPath:     os.Getenv("SCHEMA_MANIFEST_PATH"),
		TrustedIPHeader:        os.Getenv("TRUSTED_CLIENT_IP_HEADER"),
		Ark: ark.Config{
			BaseURL: value("ARK_BASE_URL", "https://ark.cn-beijing.volces.com"),
			APIKey:  os.Getenv("ARK_API_KEY"),
			Routes:  arkRoutes(arkTimeout),
		},
		TOS: media.TOSConfig{
			Endpoint:  value("TOS_ENDPOINT", "https://tos-cn-beijing.volces.com"),
			Region:    value("TOS_REGION", "cn-beijing"),
			Bucket:    os.Getenv("TOS_BUCKET"),
			AccessKey: os.Getenv("TOS_ACCESS_KEY"),
			SecretKey: os.Getenv("TOS_SECRET_KEY"),
		},
		Quota: quota.Limits{
			RequestsPerMinutePerIP:        quotaIP,
			RequestsPerMinutePerDevice:    quotaDevice,
			RequestsPerMinutePerOperation: quotaOperation,
			DailyTokensPerTransaction:     quotaDaily,
			MonthlyTokensPerTransaction:   quotaMonthly,
			MaxConcurrentPerDevice:        quotaConcurrent,
		},
		AppStore: AppStoreConfig{
			Environment:    os.Getenv("APP_STORE_ENV"),
			IssuerID:       os.Getenv("APP_STORE_ISSUER_ID"),
			KeyID:          os.Getenv("APP_STORE_KEY_ID"),
			PrivateKeyPath: os.Getenv("APP_STORE_PRIVATE_KEY_PATH"),
			RootPEMPath:    os.Getenv("APP_STORE_ROOT_PEM_PATH"),
			AppAppleID:     appAppleID,
		},
	}
	return config, nil
}

func (config Config) Validate() error {
	if config.Environment != "development" && config.Environment != "production" {
		return errors.New("APP_ENV must be development or production")
	}
	if config.AttestationEnvironment != attestation.EnvironmentDevelopment && config.AttestationEnvironment != attestation.EnvironmentProduction {
		return errors.New("APP_ATTEST_ENV must be development or production")
	}
	if config.Environment == "production" && config.AttestationEnvironment != attestation.EnvironmentProduction {
		return errors.New("production requires production App Attest")
	}
	if config.Environment == "production" {
		if err := config.AppStore.validate(); err != nil {
			return err
		}
	}
	if config.TeamID == "" || config.BundleID == "" || config.AppAttestRootPEMPath == "" {
		return errors.New("apple App Attest identity and root certificate are required")
	}
	if config.Environment == "development" && (config.DevelopmentSecret == "" || len(config.AllowedBuilds) == 0) {
		return errors.New("DEV_ACTIVATION_SECRET and ALLOWED_APP_BUILDS are required in development")
	}
	if config.StorageMode != "memory" && config.StorageMode != "mysql" {
		return errors.New("STORAGE_MODE must be memory or mysql")
	}
	if config.StorageMode == "memory" && config.Environment != "development" {
		return errors.New("memory storage is forbidden in production")
	}
	if config.StorageMode == "mysql" && (config.DatabaseDSN == "" || config.RedisURL == "" || config.PayloadEncryptionKey == "") {
		return errors.New("DATABASE_DSN, REDIS_URL, and PAYLOAD_ENCRYPTION_KEY are required")
	}
	if config.WorkerSecret == "" {
		return errors.New("WORKER_INTERNAL_SECRET is required")
	}
	if len(config.JobCapabilitySecret) < 32 {
		return errors.New("JOB_CAPABILITY_SECRET must be at least 32 bytes")
	}
	if config.SchemaManifestPath == "" {
		return errors.New("SCHEMA_MANIFEST_PATH is required")
	}
	if config.StorageMode == "mysql" && config.WorkerAsyncURL == "" {
		return errors.New("WORKER_ASYNC_URL is required for distributed storage")
	}
	if err := validateArkConfig(config.Ark); err != nil {
		return err
	}
	if config.TOS.Bucket == "" || config.TOS.AccessKey == "" || config.TOS.SecretKey == "" {
		return errors.New("tos bucket credentials are required")
	}
	return nil
}

func (config AppStoreConfig) validate() error {
	if config.Environment != "Production" && config.Environment != "Sandbox" && config.Environment != "Both" {
		return errors.New("APP_STORE_ENV must be Production, Sandbox, or Both")
	}
	if config.IssuerID == "" || config.KeyID == "" || config.PrivateKeyPath == "" || config.RootPEMPath == "" {
		return errors.New("APP_STORE_ISSUER_ID, APP_STORE_KEY_ID, APP_STORE_PRIVATE_KEY_PATH, and APP_STORE_ROOT_PEM_PATH are required")
	}
	if (config.Environment == "Production" || config.Environment == "Both") && config.AppAppleID <= 0 {
		return errors.New("APP_STORE_APP_APPLE_ID is required when Production is enabled")
	}
	return nil
}

func validateArkConfig(config ark.Config) error {
	if config.APIKey == "" {
		return errors.New("ARK_API_KEY is required")
	}
	for _, operation := range contracts.OperationValues() {
		route, ok := config.Routes[operation]
		if !ok || route.Model == "" {
			return fmt.Errorf("%s is required for Ark operation %s", arkEndpointEnvironmentKeys[operation], operation)
		}
		if route.TimeoutSeconds <= 0 || route.TimeoutSeconds > 14*60 {
			return errors.New("ARK_TIMEOUT_SECONDS must be between 1 and 840")
		}
	}
	return nil
}

var arkEndpointEnvironmentKeys = map[contracts.Operation]string{
	contracts.OperationVoiceTranscription:      "ARK_ENDPOINT_VOICE_TRANSCRIPTION",
	contracts.OperationMealPhotoCapture:        "ARK_ENDPOINT_MEAL_PHOTO_CAPTURE",
	contracts.OperationMealTextCapture:         "ARK_ENDPOINT_MEAL_TEXT_CAPTURE",
	contracts.OperationMealDecision:            "ARK_ENDPOINT_MEAL_DECISION",
	contracts.OperationDietAnalysis:            "ARK_ENDPOINT_DIET_ANALYSIS",
	contracts.OperationHealthNutritionAnalysis: "ARK_ENDPOINT_HEALTH_NUTRITION_ANALYSIS",
	contracts.OperationHealthBehaviorAnalysis:  "ARK_ENDPOINT_HEALTH_BEHAVIOR_ANALYSIS",
}

func arkRoutes(timeout int) map[contracts.Operation]ark.Route {
	routes := make(map[contracts.Operation]ark.Route)
	for _, operation := range contracts.OperationValues() {
		environmentKey := arkEndpointEnvironmentKeys[operation]
		if model := os.Getenv(environmentKey); model != "" {
			routes[operation] = ark.Route{Model: model, TimeoutSeconds: timeout}
		}
	}
	return routes
}

func value(key, fallback string) string {
	if result := strings.TrimSpace(os.Getenv(key)); result != "" {
		return result
	}
	return fallback
}

func parsedInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %s", key)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return value, nil
}

func parsedInt64Optional(key string) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
}

func splitSet(raw string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}
