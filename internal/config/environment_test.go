package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

func TestPlatformOnlyLoadsExplicitKnownApplications(t *testing.T) {
	t.Setenv("PLATFORM_APPS", "health,birthday")
	t.Setenv("JOURNAL_QUOTA_DAILY_TOKENS", "invalid-unselected-configuration")
	platform, err := loadPlatformUnchecked()
	if err != nil || len(platform.Apps) != 2 || platform.Apps[0].Registry.ID != appregistry.Health || platform.Apps[1].Registry.ID != appregistry.Birthday {
		t.Fatalf("test gateway loaded an unselected App: %v", err)
	}
	for _, invalid := range []string{"health,health", "unknown", "health,", ",birthday"} {
		t.Setenv("PLATFORM_APPS", invalid)
		if _, err := loadPlatformUnchecked(); err == nil {
			t.Fatal("invalid App selection accepted")
		}
	}
}

func TestPlatformDefaultsSeparateDevelopmentAndProductionIdentities(t *testing.T) {
	t.Setenv("PLATFORM_APPS", "health,journal,birthday")
	for _, prefix := range []string{"HEALTH", "JOURNAL", "BIRTHDAY"} {
		t.Setenv(prefix+"_APPLE_BUNDLE_ID", "")
		t.Setenv(prefix+"_API_HOSTS", "")
		t.Setenv(prefix+"_APP_ATTEST_ENV", "")
	}
	for _, environment := range []string{"development", "production"} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("APP_ENV", environment)
			platform, err := loadPlatformUnchecked()
			if err != nil {
				t.Fatal(err)
			}
			bundles := []string{"cn.tellyouwhat.healthapp", "cn.tellyouwhat.journalapp", "cn.tellyouwhat.birthday"}
			names := []string{"health", "journal", "birthday"}
			if len(platform.Apps) != len(bundles) {
				t.Fatalf("expected all three platform apps, got %d", len(platform.Apps))
			}
			for i, app := range platform.Apps {
				if app.AttestationEnvironment != attestation.EnvironmentProduction {
					t.Fatalf("%s proof realm = %s, want production for both install methods", environment, app.AttestationEnvironment)
				}
				bundle, host := bundles[i], "api."+names[i]+".tellyouwhat.cn"
				if environment == "development" {
					bundle += ".dev"
					host = "api-test." + names[i] + ".tellyouwhat.cn"
				}
				if app.Registry.BundleID != bundle || !slices.Equal(app.Registry.Hosts, []string{host}) {
					t.Fatalf("%s resolved wrong identity: %s %v", environment, app.Registry.BundleID, app.Registry.Hosts)
				}
			}
		})
	}
}

func TestAppValidationRejectsCrossEnvironmentConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, environment, bundle, host, wantError string
	}{
		{"development formal bundle", "development", "cn.tellyouwhat.healthapp", "api-test.health.tellyouwhat.cn", "APPLE_BUNDLE_ID"},
		{"production development bundle", "production", "cn.tellyouwhat.healthapp.dev", "api.health.tellyouwhat.cn", "APPLE_BUNDLE_ID"},
		{"development formal host", "development", "cn.tellyouwhat.healthapp.dev", "api.health.tellyouwhat.cn", "API_HOSTS"},
		{"normalized formal host", "development", "cn.tellyouwhat.healthapp.dev", "API.HEALTH.TELLYOUWHAT.CN.:443", "API_HOSTS"},
		{"production development host", "production", "cn.tellyouwhat.healthapp", "api-test.health.tellyouwhat.cn", "API_HOSTS"},
		{"unknown environment", "staging", "cn.tellyouwhat.healthapp.dev", "api-test.health.tellyouwhat.cn", "APP_ENV"},
		{"development", "development", "cn.tellyouwhat.healthapp.dev", "api-test.health.tellyouwhat.cn", ""},
		{"local development", "development", "cn.tellyouwhat.healthapp.dev", "localhost:8080", ""},
		{"production", "production", "cn.tellyouwhat.healthapp", "api.health.tellyouwhat.cn", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := validHealthAppConfig()
			app.Registry.BundleID = tc.bundle
			app.Registry.Hosts = []string{tc.host}
			app.AttestationEnvironment = attestation.EnvironmentProduction
			app.AppStore = validProductionAppStoreConfig("Both")
			err := app.Validate(tc.environment)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected %s error, got %v", tc.wantError, err)
			}
		})
	}
}

func TestExplicitIdentityOverridesCannotBypassEnvironmentValidation(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("HEALTH_APPLE_BUNDLE_ID", "cn.tellyouwhat.healthapp")
	t.Setenv("HEALTH_API_HOSTS", "api.health.tellyouwhat.cn")
	platform, err := loadPlatformUnchecked()
	if err != nil {
		t.Fatal(err)
	}
	app := validHealthAppConfig()
	app.Registry.BundleID = platform.Apps[0].Registry.BundleID
	app.Registry.Hosts = platform.Apps[0].Registry.Hosts
	if err := app.Validate(platform.Environment); err == nil || !strings.Contains(err.Error(), "APPLE_BUNDLE_ID") {
		t.Fatalf("formal override accepted: %v", err)
	}
}

func TestPlatformRejectsDatabaseEnvironmentMismatchBeforeConnecting(t *testing.T) {
	for _, tc := range []struct {
		name, environment, dsn string
		valid                  bool
	}{
		{"test", "development", "app_test:private-password@tcp(localhost:3306)/platform_test", true},
		{"production", "production", "app:private-password@tcp(localhost:3306)/tellyouwhat", true},
		{"test reads production", "development", "app_test:private-password@tcp(localhost:3306)/tellyouwhat", false},
		{"test uses production account", "development", "app:private-password@tcp(localhost:3306)/platform_test", false},
		{"production reads test", "production", "app:private-password@tcp(localhost:3306)/platform_test", false},
		{"production uses test account", "production", "app_test:private-password@tcp(localhost:3306)/tellyouwhat", false},
		{"case insensitive suffix", "production", "app:private-password@tcp(localhost:3306)/platform_TEST", false},
		{"root", "production", "root:private-password@tcp(localhost:3306)/tellyouwhat", false},
		{"missing database", "development", "app_test:private-password@tcp(localhost:3306)/", false},
		{"malformed", "development", "private-password", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDatabaseEnvironment(tc.dsn, tc.environment)
			if (err == nil) != tc.valid {
				t.Fatalf("expected valid=%v, got %v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-password") {
				t.Fatal("database validation leaked credentials")
			}
		})
	}
	platform := PlatformConfig{
		Environment: "development", StorageMode: "mysql",
		DatabaseDSN: "app:private-password@tcp(localhost:3306)/tellyouwhat",
		RedisURL:    "redis://localhost:6379", PayloadEncryptionKey: "synthetic",
	}
	if err := platform.Validate(); err == nil || !strings.Contains(err.Error(), "DATABASE_DSN") {
		t.Fatalf("platform bypassed database environment check: %v", err)
	}
}
