package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func adminConfigFixture(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"DATABASE_DSN": "fixture:fixture@tcp(127.0.0.1:3306)/admin_test",
		"REDIS_URL":    "redis://127.0.0.1:6379", "ADMIN_APPS": "journal",
		"ADMIN_PREVIEW_SIGNING_KEY":        base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
		"HEALTH_APP_STORE_CONNECT_ENABLED": "false", "JOURNAL_APP_STORE_CONNECT_ENABLED": "false",
		"HEALTH_APP_STORE_CONNECT_PRIVATE_KEY_PATH":  "/unavailable-health-key",
		"JOURNAL_APP_STORE_CONNECT_PRIVATE_KEY_PATH": "/unavailable-journal-key",
	} {
		t.Setenv(key, value)
	}
}

func TestAdminConfigSelectsJournalWithoutLoadingEitherAppleKey(t *testing.T) {
	adminConfigFixture(t)
	t.Setenv("HEALTH_APP_STORE_CONNECT_ENABLED", "true")
	configuration, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.apps) != 1 || configuration.apps[0].id != "journal" || configuration.apps[0].signingKey != nil {
		t.Fatal("unselected app or disabled App Store Connect was initialized")
	}
}

func TestAdminConfigDefaultAppRegistryAndRequiredAppleCredentialsRemain(t *testing.T) {
	adminConfigFixture(t)
	t.Setenv("ADMIN_APPS", "")
	configuration, err := loadConfig()
	if err != nil || len(configuration.apps) != 2 || configuration.apps[0].id != "health" || configuration.apps[1].id != "journal" {
		t.Fatal("default registry changed", err)
	}
	t.Setenv("ADMIN_APPS", "journal")
	t.Setenv("JOURNAL_APP_STORE_CONNECT_ENABLED", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("missing default Apple credentials silently disabled offers")
	}
}

func TestAdminConfigRejectsUnknownDuplicateAppsAndInvalidEnableFlags(t *testing.T) {
	for _, apps := range []string{"journal,journal", "albums", ",journal", "health,", "JOURNAL"} {
		t.Run(apps, func(t *testing.T) {
			adminConfigFixture(t)
			t.Setenv("ADMIN_APPS", apps)
			if _, err := loadConfig(); err == nil {
				t.Fatal("accepted ambiguous admin registry")
			}
		})
	}
	adminConfigFixture(t)
	t.Setenv("JOURNAL_APP_STORE_CONNECT_ENABLED", "yes")
	if _, err := loadConfig(); err == nil {
		t.Fatal("accepted an invalid enabled flag")
	}
}

func TestAdminConfigExplicitAppleClientStillLoadsTheSigningKey(t *testing.T) {
	adminConfigFixture(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"ENABLED": "true", "PRIVATE_KEY_PATH": path, "ISSUER_ID": "fixture-issuer",
		"KEY_ID": "fixture-key", "SUBSCRIPTION_ID": "fixture-subscription",
	} {
		t.Setenv("JOURNAL_APP_STORE_CONNECT_"+name, value)
	}
	configuration, err := loadConfig()
	if err != nil || len(configuration.apps) != 1 || configuration.apps[0].signingKey == nil {
		t.Fatal("enabled Apple client failed to load", err)
	}
}
