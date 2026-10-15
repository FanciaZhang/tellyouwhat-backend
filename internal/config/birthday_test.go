package config

import (
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"testing"
)

func TestBirthdayRuntimeIsOptInAndDoesNotReuseOtherAppCredentials(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("BIRTHDAY_AI_ONLY", "false")
	t.Setenv("BIRTHDAY_AI_ENABLED", "false")
	platform, err := loadPlatformUnchecked()
	if err != nil {
		t.Fatal(err)
	}
	if len(platform.Apps) != 2 {
		t.Fatal("birthday enabled without configuration")
	}
	t.Setenv("BIRTHDAY_AI_ENABLED", "true")
	t.Setenv("HEALTH_ARK_API_KEY", "health-synthetic-key")
	t.Setenv("JOURNAL_ARK_API_KEY", "journal-synthetic-key")
	t.Setenv("BIRTHDAY_ARK_API_KEY", "")
	platform, err = loadPlatformUnchecked()
	if err != nil {
		t.Fatal(err)
	}
	app := platform.Apps[2]
	if app.Registry.ID != appregistry.Birthday || app.Registry.BundleID != "cn.tellyouwhat.birthday.dev" || app.JournalAI.APIKey != "" || app.JournalAI.TimeoutSeconds != 70 {
		t.Fatal("birthday identity, credential isolation or deadline is incorrect")
	}
	if app.Registry.AllowsOperation("journal.organize") || app.Registry.AllowsOperation("birthday.reflection") || !app.Registry.AllowsOperation("birthday.revise") {
		t.Fatal("operation boundary incorrect")
	}
	if app.Validate("development") == nil {
		t.Fatal("incomplete configuration accepted")
	}
}

func TestBirthdayOnlyRuntimeKeepsOtherAppCredentialsOut(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("BIRTHDAY_AI_ONLY", "true")
	t.Setenv("BIRTHDAY_AI_ENABLED", "false")
	if _, err := loadPlatformUnchecked(); err == nil {
		t.Fatal("birthday-only runtime accepted a disabled service")
	}
	t.Setenv("BIRTHDAY_AI_ENABLED", "true")
	t.Setenv("HEALTH_ARK_API_KEY", "")
	t.Setenv("JOURNAL_ARK_API_KEY", "")
	platform, err := loadPlatformUnchecked()
	if err != nil || len(platform.Apps) != 1 || platform.Apps[0].Registry.ID != appregistry.Birthday {
		t.Fatal("isolated birthday runtime still loads another App")
	}
	platform.Environment = "development"
	platform.StorageMode = "memory"
	platform.AppAttestRootPEMPath = "synthetic-root.pem"
	platform.WorkerSecret = "synthetic-worker"
	platform.JobCapabilitySecret = "synthetic-job-capability-32-bytes-long"
	app := &platform.Apps[0]
	app.Registry.TeamID = "SYNTHETIC"
	app.AttestationEnvironment = "development"
	app.DevelopmentSecret = "synthetic-development-secret"
	app.AllowedBuilds = map[string]struct{}{"1": {}}
	app.JournalAI.APIKey, app.JournalAI.LiteModel = "synthetic-key", "synthetic-model"
	app.BirthdayPrice = platform.AICost.JournalArk
	if err := platform.Validate(); err != nil {
		t.Fatalf("text-only birthday still requires another App or media credentials: %v", err)
	}
}
