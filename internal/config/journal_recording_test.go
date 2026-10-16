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
