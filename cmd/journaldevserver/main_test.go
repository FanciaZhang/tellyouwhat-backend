package main

import (
	"strings"
	"testing"
)

func TestRefusesProductionStorageOrEnvironment(t *testing.T) {
	for _, key := range []string{"DATABASE_DSN", "REDIS_URL", "APP_ENV"} {
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
