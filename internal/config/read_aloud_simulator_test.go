package config

import (
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/journal/simulatortest"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

func TestReadAloudSimulatorConfigIsExplicitAndDevelopmentOnly(t *testing.T) {
	t.Setenv("PLATFORM_APPS", "journal")
	t.Setenv("APP_ENV", "development")
	t.Setenv("JOURNAL_READ_ALOUD_SIMULATOR_CREDENTIALS_FILE", "")
	platform, err := loadPlatformUnchecked()
	if err != nil || platform.Apps[0].ReadAloudSimulatorCredentialsFile != "" {
		t.Fatal("test authentication enabled by default", err)
	}
	t.Setenv("JOURNAL_READ_ALOUD_SIMULATOR_CREDENTIALS_FILE", "/private/not-read-during-validation/registry.json")
	platform, err = loadPlatformUnchecked()
	if err != nil {
		t.Fatal(err)
	}
	app := platform.Apps[0]
	app.Registry.TeamID = "TEAM"
	app.AllowedBuilds = map[string]struct{}{"100": {}}
	app.AttestationEnvironment = attestation.EnvironmentProduction
	app.AppStore = validProductionAppStoreConfig("Sandbox")
	app.JournalAI = JournalAIConfig{BaseURL: "https://ark.test/api/v3", APIKey: "synthetic", LiteModel: "lite", ProModel: "pro", TimeoutSeconds: 90}
	if err := app.Validate("development"); err != nil {
		t.Fatal("valid isolated test configuration rejected", err)
	}
	for _, name := range []string{"production", "health", "formal bundle", "formal host", "extra host", "relative path"} {
		t.Run(name, func(t *testing.T) {
			c := app
			c.Registry.Hosts = append([]string(nil), app.Registry.Hosts...)
			environment := "development"
			switch name {
			case "production":
				environment = "production"
			case "health":
				c.Registry.ID = appregistry.Health
			case "formal bundle":
				c.Registry.BundleID = "cn.tellyouwhat.journalapp"
			case "formal host":
				c.Registry.Hosts = []string{"api.journal.tellyouwhat.cn"}
			case "extra host":
				c.Registry.Hosts = append(c.Registry.Hosts, "localhost")
			case "relative path":
				c.ReadAloudSimulatorCredentialsFile = "registry.json"
			}
			if err := c.Validate(environment); err == nil || !strings.Contains(err.Error(), "READ_ALOUD_SIMULATOR_CREDENTIALS_FILE") {
				t.Fatal("unsafe test authentication configuration accepted", err)
			}
		})
	}
	if app.Registry.BundleID != simulatortest.BundleID || app.Registry.Hosts[0] != simulatortest.Host {
		t.Fatal("test defaults drifted")
	}
}
