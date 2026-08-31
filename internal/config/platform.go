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
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/provider/ark"
	"github.com/tellyouwhat/backend/internal/quota"
)

// PlatformConfig owns infrastructure shared by every Tellyouwhat application.
// App identity never comes from a request body or header; the gateway resolves
// it from AppConfig.Registry.Hosts before selecting an isolated runtime.
type PlatformConfig struct {
	Environment          string
	Port                 string
	StorageMode          string
	DatabaseDSN          string
	RedisURL             string
	PayloadEncryptionKey string
	AppAttestRootPEMPath string
	WorkerSecret         string
	WorkerAsyncURL       string
	JobCapabilitySecret  string
	TrustedIPHeader      string
	TOS                  media.TOSConfig
	Apps                 []AppConfig
}

type AppConfig struct {
	Registry               appregistry.App
	AttestationEnvironment attestation.Environment
	DevelopmentSecret      string
	AllowedBuilds          map[string]struct{}
	SchemaManifestPath     string
	Ark                    ark.Config
	JournalAI              JournalAIConfig
	Quota                  quota.Limits
	AppStore               AppStoreConfig
	Product                ProductConfig
}

type JournalAIConfig struct {
	BaseURL        string
	APIKey         string
	LiteModel      string
	ProModel       string
	TimeoutSeconds int
}

type ProductConfig struct {
	BillingPeriod     string
	PrivacyURL        string
	TermsURL          string
	PrivacyChoicesURL string
	SupportURL        string
}

func LoadPlatform() (PlatformConfig, error) {
	result, err := loadPlatformUnchecked()
	if err != nil {
		return PlatformConfig{}, err
	}
	if err := result.Validate(); err != nil {
		return PlatformConfig{}, err
	}
	return result, nil
}

func LoadWorkerPlatform() (PlatformConfig, error) {
	result, err := LoadPlatform()
	if err != nil {
		return PlatformConfig{}, err
	}
	if result.StorageMode != "mysql" {
		return PlatformConfig{}, errors.New("distributed worker requires mysql storage")
	}
	return result, nil
}

func loadPlatformUnchecked() (PlatformConfig, error) {
	environment := value("APP_ENV", "development")
	commonTeamID := strings.TrimSpace(os.Getenv("APPLE_TEAM_ID"))
	result := PlatformConfig{
		Environment:          environment,
		Port:                 value("PORT", "8080"),
		StorageMode:          value("STORAGE_MODE", "mysql"),
		DatabaseDSN:          os.Getenv("DATABASE_DSN"),
		RedisURL:             os.Getenv("REDIS_URL"),
		PayloadEncryptionKey: os.Getenv("PAYLOAD_ENCRYPTION_KEY"),
		AppAttestRootPEMPath: os.Getenv("APP_ATTEST_ROOT_PEM_PATH"),
		WorkerSecret:         os.Getenv("WORKER_INTERNAL_SECRET"),
		WorkerAsyncURL:       os.Getenv("WORKER_ASYNC_URL"),
		JobCapabilitySecret:  os.Getenv("JOB_CAPABILITY_SECRET"),
		TrustedIPHeader:      os.Getenv("TRUSTED_CLIENT_IP_HEADER"),
		TOS: media.TOSConfig{
			Endpoint:  value("TOS_ENDPOINT", "https://tos-cn-beijing.volces.com"),
			Region:    value("TOS_REGION", "cn-beijing"),
			Bucket:    os.Getenv("TOS_BUCKET"),
			AccessKey: os.Getenv("TOS_ACCESS_KEY"),
			SecretKey: os.Getenv("TOS_SECRET_KEY"),
		},
	}

	health, err := loadPlatformApp("HEALTH", appDefaults{
		ID: appregistry.Health, DisplayName: "告你健康", Host: "api.health.tellyouwhat.cn",
		BundleID: "cn.tellyouwhat.healthapp", ProductID: "health.ai.subscription.monthly",
		OperationPrefix: "health.", PrivacyBaseURL: "https://health.tellyouwhat.cn",
	}, environment, commonTeamID)
	if err != nil {
		return PlatformConfig{}, err
	}
	journal, err := loadPlatformApp("JOURNAL", appDefaults{
		ID: appregistry.Journal, DisplayName: "告你手记", Host: "api.journal.tellyouwhat.cn",
		BundleID: "cn.tellyouwhat.journalapp", ProductID: "journal.ai.subscription.monthly",
		OperationPrefix: "journal.", PrivacyBaseURL: "https://journal.tellyouwhat.cn",
	}, environment, commonTeamID)
	if err != nil {
		return PlatformConfig{}, err
	}
	result.Apps = []AppConfig{health, journal}
	return result, nil
}

type appDefaults struct {
	ID              appregistry.AppID
	DisplayName     string
	Host            string
	BundleID        string
	ProductID       string
	OperationPrefix string
	PrivacyBaseURL  string
}

func loadPlatformApp(prefix string, defaults appDefaults, environment, commonTeamID string) (AppConfig, error) {
	attestationEnvironment := attestation.Environment(prefixedValue(prefix, "APP_ATTEST_ENV", environment))
	timeout, err := prefixedInt(prefix, "ARK_TIMEOUT_SECONDS", 90)
	if err != nil {
		return AppConfig{}, err
	}
	quotaLimits, err := loadPrefixedQuota(prefix)
	if err != nil {
		return AppConfig{}, err
	}
	appAppleID, err := prefixedInt64Optional(prefix, "APP_STORE_APP_APPLE_ID")
	if err != nil {
		return AppConfig{}, err
	}
	base := defaults.PrivacyBaseURL
	config := AppConfig{
		Registry: appregistry.App{
			ID:                     defaults.ID,
			DisplayName:            prefixedValue(prefix, "DISPLAY_NAME", defaults.DisplayName),
			Hosts:                  splitList(prefixedValue(prefix, "API_HOSTS", defaults.Host)),
			TeamID:                 prefixedValue(prefix, "APPLE_TEAM_ID", commonTeamID),
			BundleID:               prefixedValue(prefix, "APPLE_BUNDLE_ID", defaults.BundleID),
			AppAppleID:             appAppleID,
			ManagedAIProductID:     prefixedValue(prefix, "MANAGED_AI_PRODUCT_ID", defaults.ProductID),
			AllowedOperationPrefix: defaults.OperationPrefix,
		},
		AttestationEnvironment: attestationEnvironment,
		DevelopmentSecret:      prefixedValue(prefix, "DEV_ACTIVATION_SECRET", ""),
		AllowedBuilds:          splitSet(prefixedValue(prefix, "ALLOWED_APP_BUILDS", "")),
		SchemaManifestPath:     prefixedValue(prefix, "SCHEMA_MANIFEST_PATH", ""),
		Quota:                  quotaLimits,
		AppStore: AppStoreConfig{
			Environment:    prefixedValue(prefix, "APP_STORE_ENV", ""),
			IssuerID:       prefixedValue(prefix, "APP_STORE_ISSUER_ID", ""),
			KeyID:          prefixedValue(prefix, "APP_STORE_KEY_ID", ""),
			PrivateKeyPath: prefixedValue(prefix, "APP_STORE_PRIVATE_KEY_PATH", ""),
			RootPEMPath:    prefixedValue(prefix, "APP_STORE_ROOT_PEM_PATH", ""),
			AppAppleID:     appAppleID,
		},
		Product: ProductConfig{
			BillingPeriod:     prefixedValue(prefix, "BILLING_PERIOD", "P1M"),
			PrivacyURL:        prefixedValue(prefix, "PRIVACY_URL", base+"/privacy"),
			TermsURL:          prefixedValue(prefix, "TERMS_URL", base+"/terms"),
			PrivacyChoicesURL: prefixedValue(prefix, "PRIVACY_CHOICES_URL", base+"/privacy-choices"),
			SupportURL:        prefixedValue(prefix, "SUPPORT_URL", base+"/support"),
		},
	}
	if defaults.ID == appregistry.Health {
		config.Ark = ark.Config{
			BaseURL: prefixedValue(prefix, "ARK_BASE_URL", "https://ark.cn-beijing.volces.com"),
			APIKey:  prefixedValue(prefix, "ARK_API_KEY", ""),
			Routes:  prefixedArkRoutes(prefix, timeout),
		}
	}
	if defaults.ID == appregistry.Journal {
		config.JournalAI = JournalAIConfig{
			BaseURL:        prefixedValue(prefix, "ARK_BASE_URL", "https://ark.cn-beijing.volces.com/api/v3"),
			APIKey:         prefixedValue(prefix, "ARK_API_KEY", ""),
			LiteModel:      prefixedValue(prefix, "ARK_LITE_MODEL_ID", ""),
			ProModel:       prefixedValue(prefix, "ARK_PRO_MODEL_ID", ""),
			TimeoutSeconds: timeout,
		}
	}
	return config, nil
}

func (config PlatformConfig) Validate() error {
	if config.Environment != "development" && config.Environment != "production" {
		return errors.New("APP_ENV must be development or production")
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
	if config.AppAttestRootPEMPath == "" {
		return errors.New("APP_ATTEST_ROOT_PEM_PATH is required")
	}
	if config.WorkerSecret == "" {
		return errors.New("WORKER_INTERNAL_SECRET is required")
	}
	if config.StorageMode == "mysql" && config.WorkerAsyncURL == "" {
		return errors.New("WORKER_ASYNC_URL is required for distributed storage")
	}
	if len(config.JobCapabilitySecret) < 32 {
		return errors.New("JOB_CAPABILITY_SECRET must be at least 32 bytes")
	}
	if config.TOS.Bucket == "" || config.TOS.AccessKey == "" || config.TOS.SecretKey == "" {
		return errors.New("TOS bucket credentials are required")
	}
	registryEntries := make([]appregistry.App, 0, len(config.Apps))
	for _, app := range config.Apps {
		if err := app.Validate(config.Environment); err != nil {
			return fmt.Errorf("app %s: %w", app.Registry.ID, err)
		}
		registryEntries = append(registryEntries, app.Registry)
	}
	_, err := appregistry.New(registryEntries)
	return err
}

func (config AppConfig) Validate(environment string) error {
	if err := config.Registry.Validate(); err != nil {
		return err
	}
	if config.AttestationEnvironment != attestation.EnvironmentDevelopment && config.AttestationEnvironment != attestation.EnvironmentProduction {
		return errors.New("APP_ATTEST_ENV must be development or production")
	}
	if environment == "production" && config.AttestationEnvironment != attestation.EnvironmentProduction {
		return errors.New("production requires production App Attest")
	}
	if environment == "development" && (config.DevelopmentSecret == "" || len(config.AllowedBuilds) == 0) {
		return errors.New("DEV_ACTIVATION_SECRET and ALLOWED_APP_BUILDS are required in development")
	}
	if environment == "production" {
		if err := config.AppStore.validate(); err != nil {
			return err
		}
	}
	switch config.Registry.ID {
	case appregistry.Health:
		if config.SchemaManifestPath == "" {
			return errors.New("SCHEMA_MANIFEST_PATH is required")
		}
		if err := validateArkConfig(config.Ark); err != nil {
			return err
		}
	case appregistry.Journal:
		if config.JournalAI.BaseURL == "" || config.JournalAI.APIKey == "" || config.JournalAI.LiteModel == "" || config.JournalAI.ProModel == "" {
			return errors.New("journal Ark Responses API configuration is incomplete")
		}
		if config.JournalAI.TimeoutSeconds <= 0 || config.JournalAI.TimeoutSeconds > 14*60 {
			return errors.New("journal ARK_TIMEOUT_SECONDS must be between 1 and 840")
		}
	default:
		return errors.New("unsupported app")
	}
	return nil
}

func loadPrefixedQuota(prefix string) (quota.Limits, error) {
	keys := []struct {
		name     string
		fallback int
		target   *int
	}{}
	result := quota.Limits{}
	keys = append(keys,
		struct {
			name     string
			fallback int
			target   *int
		}{"QUOTA_REQUESTS_PER_MINUTE_IP", 30, &result.RequestsPerMinutePerIP},
		struct {
			name     string
			fallback int
			target   *int
		}{"QUOTA_REQUESTS_PER_MINUTE_DEVICE", 20, &result.RequestsPerMinutePerDevice},
		struct {
			name     string
			fallback int
			target   *int
		}{"QUOTA_REQUESTS_PER_MINUTE_OPERATION", 10, &result.RequestsPerMinutePerOperation},
		struct {
			name     string
			fallback int
			target   *int
		}{"QUOTA_DAILY_TOKENS", 300_000, &result.DailyTokensPerTransaction},
		struct {
			name     string
			fallback int
			target   *int
		}{"QUOTA_MONTHLY_TOKENS", 5_000_000, &result.MonthlyTokensPerTransaction},
		struct {
			name     string
			fallback int
			target   *int
		}{"QUOTA_MAX_CONCURRENT_DEVICE", 2, &result.MaxConcurrentPerDevice},
	)
	for _, key := range keys {
		value, err := prefixedInt(prefix, key.name, key.fallback)
		if err != nil {
			return quota.Limits{}, err
		}
		*key.target = value
	}
	return result, nil
}

func prefixedArkRoutes(prefix string, timeout int) map[contracts.Operation]ark.Route {
	routes := make(map[contracts.Operation]ark.Route)
	for _, operation := range contracts.OperationValues() {
		key := arkEndpointEnvironmentKeys[operation]
		if model := prefixedValue(prefix, key, ""); model != "" {
			routes[operation] = ark.Route{Model: model, TimeoutSeconds: timeout}
		}
	}
	return routes
}

func prefixedValue(prefix, key, fallback string) string {
	if result := strings.TrimSpace(os.Getenv(prefix + "_" + key)); result != "" {
		return result
	}
	return fallback
}

func prefixedInt(prefix, key string, fallback int) (int, error) {
	raw := prefixedValue(prefix, key, "")
	if raw == "" {
		return fallback, nil
	}
	return parsePositiveInt(prefix+"_"+key, raw)
}

func prefixedInt64Optional(prefix, key string) (int64, error) {
	raw := prefixedValue(prefix, key, "")
	if raw == "" {
		return 0, nil
	}
	return parsePositiveInt64(prefix+"_"+key, raw)
}

func parsePositiveInt(key, raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
}

func parsePositiveInt64(key, raw string) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
}

func splitList(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}
