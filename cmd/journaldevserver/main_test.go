package main

import (
	"strings"
	"testing"
)

func TestRefusesProductionStorageOrEnvironment(t *testing.T) {
	for _, key := range []string{"DATABASE_DSN", "JOURNAL_PROMPT_DATABASE_DSN", "REDIS_URL", "APP_ENV"} {
		t.Run(key, func(t *testing.T) {
			value := "configured"
			if key == "APP_ENV" {
				value = "production"
			}
			t.Setenv(key, value)
			if err := run(); err == nil || !strings.Contains(err.Error(), "refusing production") {
				t.Fatal("production configuration did not fail closed")
			}
		})
	}
}

func TestRecoveryVerificationCannotOpenLiveStateOrUseConnectionCredentials(t *testing.T) {
	for _, path := range []string{"", "/state", "/opt/tellyouwhat/journal-development/state"} {
		t.Setenv("JOURNAL_DEVELOPMENT_STATE_DIR", path)
		if err := verifyRecovery(); err == nil || !strings.Contains(err.Error(), "isolated copy") {
			t.Fatal("recovery verification accepted live or unspecified state")
		}
	}
	t.Setenv("JOURNAL_DEVELOPMENT_STATE_DIR", "/restore/state")
	t.Setenv("JOURNAL_PROMPT_FILE", "/restore/config/prompt.json")
	for _, key := range []string{"JOURNAL_DEVELOPMENT_TOKEN", "JOURNAL_PROMPT_DATABASE_DSN", "DATABASE_DSN", "REDIS_URL"} {
		t.Setenv(key, "never-use-live-access")
		if err := verifyRecovery(); err == nil || !strings.Contains(err.Error(), "without connection credentials") {
			t.Fatal("recovery verification accepted live credentials")
		}
		t.Setenv(key, "")
	}
}
