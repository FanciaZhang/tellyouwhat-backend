package config

import (
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

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
