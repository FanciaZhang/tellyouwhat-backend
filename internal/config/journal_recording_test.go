package config

import (
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"strings"
	"testing"
	"time"
)

func TestRecordingConfigurationKeepsRetentionWhenDisabledAndRejectsWrongEnvironment(t *testing.T) {
	t.Setenv("JOURNAL_RECORDING_ROOT", "")
	t.Setenv("JOURNAL_RECORDING_ENABLED", "")
	if c, err := LoadJournalRecording(""); err != nil || c != nil {
		t.Fatal("unused recording configuration required", c, err)
	}
	t.Setenv("JOURNAL_RECORDING_ENABLED", "true")
	if _, err := LoadJournalRecording("development"); err == nil {
		t.Fatal("enabled without durable root")
	}
	t.Setenv("JOURNAL_RECORDING_ROOT", "/var/lib/journal/development")
	if c, err := LoadJournalRecording("development"); err != nil || c == nil || !c.Enabled {
		t.Fatal(c, err)
	}
	t.Setenv("JOURNAL_RECORDING_ENABLED", "false")
	if c, err := LoadJournalRecording("development"); err != nil || c == nil || c.Enabled {
		t.Fatal("disabling generation also disabled retention", c, err)
	}
	for _, root := range []string{"relative/development", "/var/lib/journal/production", "/var/lib/../development"} {
		t.Setenv("JOURNAL_RECORDING_ROOT", root)
		if _, err := LoadJournalRecording("development"); err == nil {
			t.Fatal("unsafe root accepted", root)
		}
	}
	t.Setenv("JOURNAL_RECORDING_ENABLED", "yes")
	if _, err := LoadJournalRecording("development"); err == nil {
		t.Fatal("ambiguous switch accepted")
	}
}

func TestRecordingRuntimeRejectsMissingVoiceDurabilityOrBudget(t *testing.T) {
	for _, missing := range []string{"voice", "mysql", "budget"} {
		t.Run(missing, func(t *testing.T) {
			c := PlatformConfig{Environment: "development", StorageMode: "mysql", DatabaseDSN: "journal_test:synthetic@tcp(localhost:3306)/platform_test", RedisURL: "redis://localhost", PayloadEncryptionKey: "synthetic", AppAttestRootPEMPath: "fixture.pem", WorkerSecret: "synthetic", WorkerAsyncURL: "http://worker", JobCapabilitySecret: strings.Repeat("s", 32), TOS: media.TOSConfig{Bucket: "journal-test", AccessKey: "synthetic", SecretKey: "synthetic"}, Apps: []AppConfig{{Registry: appregistry.App{ID: appregistry.Journal}, JournalRecording: &JournalRecordingConfig{Enabled: true}, VoiceEnabled: true}}, AICost: AICostConfig{Limits: costcontrol.Limits{MonthlyBudgetNanos: 100, MaxConcurrent: 1, LeaseDuration: time.Minute}, HealthArk: costcontrol.TokenPrice{InputNanosPerMillionTokens: 1, OutputNanosPerMillionTokens: 1}, JournalArk: costcontrol.TokenPrice{InputNanosPerMillionTokens: 1, OutputNanosPerMillionTokens: 1}, JournalSpeech: costcontrol.DurationPrice{NanosPerHour: 1}}}
			switch missing {
			case "voice":
				c.Apps[0].VoiceEnabled = false
			case "mysql":
				c.StorageMode = "memory"
			case "budget":
				c.AICost.Limits.MonthlyBudgetNanos = 0
			}
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "Journal recordings require") {
				t.Fatal("wrong admission result", err)
			}
		})
	}
}

func TestWorkerDoesNotRequireGatewayRecordingStorage(t *testing.T) {
	for key, value := range map[string]string{
		"APP_ENV": "development", "PLATFORM_APPS": "journal", "APPLE_TEAM_ID": "TEAM",
		"STORAGE_MODE": "mysql", "DATABASE_DSN": "journal_test:synthetic@tcp(localhost:3306)/platform_test",
		"REDIS_URL": "redis://localhost", "PAYLOAD_ENCRYPTION_KEY": "synthetic",
		"APP_ATTEST_ROOT_PEM_PATH": "fixture.pem", "WORKER_INTERNAL_SECRET": "synthetic",
		"WORKER_ASYNC_URL": "http://worker", "JOB_CAPABILITY_SECRET": strings.Repeat("s", 32),
		"TOS_BUCKET": "journal-test", "TOS_ACCESS_KEY": "synthetic", "TOS_SECRET_KEY": "synthetic",
		"AI_PROJECT_MONTHLY_BUDGET_CNY": "10", "JOURNAL_ALLOWED_APP_BUILDS": "20",
		"JOURNAL_ARK_API_KEY": "synthetic", "JOURNAL_ARK_LITE_MODEL_ID": "lite", "JOURNAL_ARK_PRO_MODEL_ID": "pro",
		"JOURNAL_VOICE_ENABLED": "true", "JOURNAL_RECORDING_ENABLED": "true", "JOURNAL_RECORDING_ROOT": "",
	} {
		t.Setenv(key, value)
	}
	worker, err := LoadWorkerPlatform()
	if err != nil || worker.Apps[0].JournalRecording != nil {
		t.Fatal("worker required API-only recording storage", err)
	}
	if _, err := LoadPlatform(); err == nil || !strings.Contains(err.Error(), "JOURNAL_RECORDING_ROOT") {
		t.Fatal("gateway accepted enabled recordings without storage", err)
	}
	t.Setenv("JOURNAL_RECORDING_ROOT", "/var/lib/journal/development")
	for _, enabled := range []string{"true", "false"} {
		t.Setenv("JOURNAL_RECORDING_ENABLED", enabled)
		gateway, err := LoadPlatform()
		if err != nil || gateway.Apps[0].JournalRecording == nil || gateway.Apps[0].JournalRecording.Enabled != (enabled == "true") {
			t.Fatal("gateway lost recording or retention configuration", err)
		}
	}
	t.Setenv("DATABASE_DSN", "journal:synthetic@tcp(localhost:3306)/production")
	if _, err := LoadWorkerPlatform(); err == nil || !strings.Contains(err.Error(), "DATABASE_DSN") {
		t.Fatal("worker skipped shared configuration validation", err)
	}
}
