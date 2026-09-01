package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/provider/ark"
	"github.com/tellyouwhat/backend/internal/quota"
)

func TestDevelopmentAppRequiresExplicitBuildAllowlist(t *testing.T) {
	t.Parallel()

	app := validHealthAppConfig()
	app.AllowedBuilds = nil
	if err := app.Validate("development"); err == nil || !strings.Contains(err.Error(), "ALLOWED_APP_BUILDS") {
		t.Fatalf("expected explicit development build allowlist, got %v", err)
	}
	app.AllowedBuilds = map[string]struct{}{"100": {}}
	if err := app.Validate("development"); err != nil {
		t.Fatalf("valid development app rejected: %v", err)
	}
}

func TestHealthAppRequiresEveryFixedArkRouteWithinSynchronousLimit(t *testing.T) {
	t.Parallel()

	app := validHealthAppConfig()
	delete(app.Ark.Routes, contracts.OperationDietAnalysis)
	if err := app.Validate("development"); err == nil || !strings.Contains(err.Error(), "ARK_ENDPOINT_DIET_ANALYSIS") {
		t.Fatalf("missing fixed Ark route did not identify its module configuration: %v", err)
	}
	app = validHealthAppConfig()
	route := app.Ark.Routes[contracts.OperationMealDecision]
	route.TimeoutSeconds = 15 * 60
	app.Ark.Routes[contracts.OperationMealDecision] = route
	if err := app.Validate("development"); err == nil || !strings.Contains(err.Error(), "840") {
		t.Fatalf("timeout beyond the synchronous budget was accepted: %v", err)
	}
}

func TestPrefixedArkRoutesKeepEveryHealthModuleIndependent(t *testing.T) {
	for index, operation := range contracts.OperationValues() {
		environmentKey, ok := arkEndpointEnvironmentKeys[operation]
		if !ok || environmentKey == "" {
			t.Fatalf("missing explicit endpoint key for %s", operation)
		}
		t.Setenv("HEALTH_"+environmentKey, fmt.Sprintf("ep-module-%d", index))
	}

	routes := prefixedArkRoutes("HEALTH", 180)
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

func TestProductionAppRequiresAppStoreServerCredentials(t *testing.T) {
	t.Parallel()

	app := validHealthAppConfig()
	app.AttestationEnvironment = attestation.EnvironmentProduction
	app.DevelopmentSecret = ""
	app.AllowedBuilds = nil
	if err := app.Validate("production"); err == nil || !strings.Contains(err.Error(), "APP_STORE") {
		t.Fatalf("production accepted missing App Store credentials: %v", err)
	}
	app.AppStore = validProductionAppStoreConfig("Production")
	if err := app.Validate("production"); err != nil {
		t.Fatalf("valid production app rejected: %v", err)
	}
}

func TestProductionAppAcceptsBothAppStoreEnvironments(t *testing.T) {
	t.Parallel()

	app := validHealthAppConfig()
	app.AttestationEnvironment = attestation.EnvironmentProduction
	app.DevelopmentSecret = ""
	app.AllowedBuilds = nil
	app.AppStore = validProductionAppStoreConfig("Both")
	if err := app.Validate("production"); err != nil {
		t.Fatalf("dual-environment App Store config rejected: %v", err)
	}
	app.AppStore.AppAppleID = 0
	if err := app.Validate("production"); err == nil || !strings.Contains(err.Error(), "APP_STORE_APP_APPLE_ID") {
		t.Fatalf("dual-environment config accepted missing production app ID: %v", err)
	}
}

func validHealthAppConfig() AppConfig {
	routes := make(map[contracts.Operation]ark.Route, len(contracts.OperationValues()))
	allowedOperations := make([]string, 0, len(contracts.OperationValues()))
	for _, operation := range contracts.OperationValues() {
		routes[operation] = ark.Route{Model: "ep-" + string(operation), TimeoutSeconds: 90}
		allowedOperations = append(allowedOperations, string(operation))
	}
	freeDailyTokens := 6 * contracts.MaxFreeRecognitionSessionReservationTokens
	return AppConfig{
		Registry: appregistry.App{
			ID: appregistry.Health, DisplayName: "告你健康", Hosts: []string{"api.health.test"},
			TeamID: "TEAM", BundleID: "health.bundle", ManagedAIProductID: "health.premium.subscription.monthly",
			AllowedOperations: allowedOperations,
		},
		AttestationEnvironment: attestation.EnvironmentDevelopment,
		DevelopmentSecret:      "development-secret",
		AllowedBuilds:          map[string]struct{}{"100": {}},
		ManagedAIProductIDs:    []string{"health.premium.subscription.monthly", "health.premium.subscription.annual"},
		SchemaManifestPath:     "/config/schema-manifest.json",
		Ark:                    ark.Config{BaseURL: "https://ark.test", APIKey: "secret", Routes: routes},
		FreeRecognitionQuota: quota.Limits{
			DailyTokensPerTransaction: freeDailyTokens, MonthlyTokensPerTransaction: freeDailyTokens * 31,
		},
	}
}

func TestAppRequiresPrimaryProductInAllowlist(t *testing.T) {
	t.Parallel()

	app := validHealthAppConfig()
	app.ManagedAIProductIDs = []string{"health.premium.subscription.annual"}
	if err := app.Validate("development"); err == nil || !strings.Contains(err.Error(), "MANAGED_AI_PRODUCT_ID") {
		t.Fatalf("missing primary product was accepted: %v", err)
	}
}

func validProductionAppStoreConfig(environment string) AppStoreConfig {
	return AppStoreConfig{
		Environment: environment, IssuerID: "issuer-id", KeyID: "key-id",
		PrivateKeyPath: "/secrets/SubscriptionKey.p8", RootPEMPath: "/config/apple-roots.pem",
		AppAppleID: 1234567890,
	}
}
