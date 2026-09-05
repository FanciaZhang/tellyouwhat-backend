package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/provider/ark"
	"github.com/tellyouwhat/backend/internal/quota"
)

func TestFreeRecognitionConfigurationCoversBoundedOutputReservations(t *testing.T) {
	t.Setenv("HEALTH_FREE_RECOGNITION_DAILY_TOKENS", "")
	t.Setenv("HEALTH_FREE_RECOGNITION_MONTHLY_TOKENS", "")
	limits, err := loadFreeRecognitionQuota("HEALTH", quota.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if limits.DailyTokensPerTransaction != 5_117_952 || limits.MonthlyTokensPerTransaction != 158_656_512 {
		t.Fatalf("default free quota does not cover bounded output reservations: %+v", limits)
	}
	routes := make(map[contracts.Operation]ark.Route)
	for _, operation := range contracts.OperationValues() {
		routes[operation] = ark.Route{Model: "fixture-model", TimeoutSeconds: 90}
	}
	app := AppConfig{
		Registry: appregistry.App{ID: appregistry.Health, DisplayName: "告你健康", Hosts: []string{"api.health.test"},
			TeamID: "TEAM", BundleID: "health.bundle", ManagedAIProductID: "health.ai.monthly", AllowedOperationPrefix: "health."},
		AttestationEnvironment: attestation.EnvironmentDevelopment, DevelopmentSecret: "fixture-development-secret",
		AllowedBuilds: map[string]struct{}{"100": {}}, ManagedAIProductIDs: []string{"health.ai.monthly"},
		SchemaManifestPath: "fixture-manifest", Ark: ark.Config{BaseURL: "https://ark.test", APIKey: "fixture", Routes: routes},
		FreeRecognitionQuota: limits,
	}
	if err := app.Validate("development"); err != nil {
		t.Fatalf("matching capped-output configuration rejected: %v", err)
	}
	app.FreeRecognitionQuota.DailyTokensPerTransaction = 4_749_312
	if err := app.Validate("development"); err == nil || !strings.Contains(err.Error(), "5117952") {
		t.Fatalf("old quota was silently accepted below required reservation capacity: %v", err)
	}
}

func TestJournalDefaultPlansRemainSeparateFromHealth(t *testing.T) {
	t.Setenv("JOURNAL_MANAGED_AI_PRODUCT_ID", "")
	t.Setenv("JOURNAL_MANAGED_AI_PRODUCT_IDS", "")
	platform, err := loadPlatformUnchecked()
	if err != nil {
		t.Fatalf("load platform: %v", err)
	}
	for _, app := range platform.Apps {
		if app.Registry.ID != appregistry.Journal {
			continue
		}
		if !slices.Equal(app.ManagedAIProductIDs, []string{
			"journal.ai.subscription.monthly", "journal.ai.subscription.annual",
		}) {
			t.Fatalf("unexpected Journal subscription allowlist: %v", app.ManagedAIProductIDs)
		}
		return
	}
	t.Fatal("Journal app is missing")
}

func TestJournalAIConfigRetainsConfiguredTimeout(t *testing.T) {
	t.Setenv("JOURNAL_ARK_TIMEOUT_SECONDS", "135")

	app, err := loadPlatformApp("JOURNAL", appDefaults{
		ID: appregistry.Journal, DisplayName: "告你手记", Host: "api.journal.test",
		BundleID: "journal.bundle", ProductID: "journal.ai.monthly",
		OperationPrefix: "journal.", PrivacyBaseURL: "https://journal.test",
	}, "development", "TEAM")
	if err != nil {
		t.Fatalf("load journal config: %v", err)
	}
	if app.JournalAI.TimeoutSeconds != 135 {
		t.Fatalf("journal timeout = %d, want 135", app.JournalAI.TimeoutSeconds)
	}
}

func TestJournalAIConfigRejectsTimeoutBeyondSynchronousBudget(t *testing.T) {
	app := AppConfig{
		Registry: appregistry.App{
			ID: appregistry.Journal, DisplayName: "告你手记", Hosts: []string{"api.journal.test"},
			TeamID: "TEAM", BundleID: "journal.bundle", ManagedAIProductID: "journal.ai.monthly",
			AllowedOperationPrefix: "journal.",
		},
		AttestationEnvironment: attestation.EnvironmentDevelopment,
		DevelopmentSecret:      "development-secret",
		AllowedBuilds:          map[string]struct{}{"100": {}},
		ManagedAIProductIDs:    []string{"journal.ai.monthly"},
		JournalAI: JournalAIConfig{
			BaseURL: "https://ark.test/api/v3", APIKey: "secret",
			LiteModel: "lite", ProModel: "pro", TimeoutSeconds: 90,
		},
	}
	if err := app.Validate("development"); err != nil {
		t.Fatalf("valid journal config rejected: %v", err)
	}
	app.JournalAI.TimeoutSeconds = 14*60 + 1
	if err := app.Validate("development"); err == nil || !strings.Contains(err.Error(), "840") {
		t.Fatalf("oversized journal timeout accepted: %v", err)
	}
}
