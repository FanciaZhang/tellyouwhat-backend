package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/provider/ark"
	"github.com/tellyouwhat/backend/internal/quota"
)

func TestDevelopmentConfigRequiresExplicitBuildAllowlist(t *testing.T) {
	t.Parallel()

	config := validDevelopmentConfig()
	config.AllowedBuilds = nil
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "ALLOWED_APP_BUILDS") {
		t.Fatalf("expected explicit development build allowlist, got %v", err)
	}
	config.AllowedBuilds = map[string]struct{}{"100": {}}
	if err := config.Validate(); err != nil {
		t.Fatalf("valid development config rejected: %v", err)
	}
}

func TestConfigRequiresEveryFixedArkRouteWithinSynchronousLimit(t *testing.T) {
	t.Parallel()

	config := validDevelopmentConfig()
	delete(config.Ark.Routes, contracts.OperationDietAnalysis)
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "ARK_ENDPOINT_DIET_ANALYSIS") {
		t.Fatalf("missing fixed Ark route did not identify its module configuration: %v", err)
	}
	config = validDevelopmentConfig()
	route := config.Ark.Routes[contracts.OperationMealDecision]
	route.TimeoutSeconds = 15 * 60
	config.Ark.Routes[contracts.OperationMealDecision] = route
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "840") {
		t.Fatalf("timeout beyond the veFaaS synchronous budget was accepted: %v", err)
	}
}

func TestArkRoutesKeepEveryModuleEndpointIndependent(t *testing.T) {
	for index, operation := range contracts.OperationValues() {
		environmentKey, ok := arkEndpointEnvironmentKeys[operation]
		if !ok || environmentKey == "" {
			t.Fatalf("missing explicit endpoint key for %s", operation)
		}
		t.Setenv(environmentKey, fmt.Sprintf("ep-module-%d", index))
	}

	routes := arkRoutes(180)
	if len(routes) != len(contracts.OperationValues()) {
		t.Fatalf("got %d Ark routes, want %d", len(routes), len(contracts.OperationValues()))
	}
	for index, operation := range contracts.OperationValues() {
		route := routes[operation]
		if want := fmt.Sprintf("ep-module-%d", index); route.Model != want {
			t.Fatalf("%s route = %q, want %q", operation, route.Model, want)
		}
		if route.TimeoutSeconds != 180 {
			t.Fatalf("%s timeout = %d, want 180", operation, route.TimeoutSeconds)
		}
	}
}

func TestProductionConfigRequiresAppStoreServerCredentials(t *testing.T) {
	t.Parallel()

	config := validDevelopmentConfig()
	config.Environment = "production"
	config.StorageMode = "mysql"
	config.DatabaseDSN = "health:password@tcp(mysql:3306)/health"
	config.RedisURL = "redis://example"
	config.PayloadEncryptionKey = "encryption-key"
	config.WorkerAsyncURL = "https://worker.example"
	config.AttestationEnvironment = attestation.EnvironmentProduction
	config.DevelopmentSecret = ""
	config.AllowedBuilds = nil
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "APP_STORE") {
		t.Fatalf("production accepted missing App Store credentials: %v", err)
	}
	config.AppStore = AppStoreConfig{
		Environment:    "Production",
		IssuerID:       "issuer-id",
		KeyID:          "key-id",
		PrivateKeyPath: "/secrets/SubscriptionKey.p8",
		RootPEMPath:    "/config/apple-roots.pem",
		AppAppleID:     1234567890,
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("valid production config rejected: %v", err)
	}
}

func TestProductionConfigAcceptsBothAppStoreEnvironments(t *testing.T) {
	t.Parallel()

	config := validDevelopmentConfig()
	config.Environment = "production"
	config.StorageMode = "mysql"
	config.DatabaseDSN = "health:password@tcp(mysql:3306)/health"
	config.RedisURL = "redis://example"
	config.PayloadEncryptionKey = "encryption-key"
	config.WorkerAsyncURL = "https://worker.example"
	config.AttestationEnvironment = attestation.EnvironmentProduction
	config.DevelopmentSecret = ""
	config.AllowedBuilds = nil
	config.AppStore = AppStoreConfig{
		Environment:    "Both",
		IssuerID:       "issuer-id",
		KeyID:          "key-id",
		PrivateKeyPath: "/secrets/SubscriptionKey.p8",
		RootPEMPath:    "/config/apple-roots.pem",
		AppAppleID:     1234567890,
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("dual-environment App Store config rejected: %v", err)
	}
	config.AppStore.AppAppleID = 0
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "APP_STORE_APP_APPLE_ID") {
		t.Fatalf("dual-environment config accepted missing production app ID: %v", err)
	}
}

func validDevelopmentConfig() Config {
	routes := make(map[contracts.Operation]ark.Route, len(contracts.OperationValues()))
	for _, operation := range contracts.OperationValues() {
		routes[operation] = ark.Route{Model: "ep-" + string(operation), TimeoutSeconds: 90}
	}
	return Config{
		Environment: "development", StorageMode: "memory",
		TeamID: "TEAMID", BundleID: "cn.tellyouwhat.healthapp",
		AttestationEnvironment: attestation.EnvironmentDevelopment,
		AppAttestRootPEMPath:   "/secrets/app-attest-root.pem",
		DevelopmentSecret:      "development-secret",
		AllowedBuilds:          map[string]struct{}{"100": {}},
		WorkerSecret:           "worker-secret",
		JobCapabilitySecret:    strings.Repeat("c", 32),
		SchemaManifestPath:     "/config/schema-manifest.json",
		Ark:                    ark.Config{APIKey: "ark-secret", Routes: routes},
		TOS:                    media.TOSConfig{Bucket: "bucket", AccessKey: "access", SecretKey: "secret"},
		FreeRecognitionQuota: quota.Limits{
			DailyTokensPerTransaction:   6 * contracts.MaxFreeRecognitionSessionReservationTokens,
			MonthlyTokensPerTransaction: 31 * 6 * contracts.MaxFreeRecognitionSessionReservationTokens,
		},
	}
}
