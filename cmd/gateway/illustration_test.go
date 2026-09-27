package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func TestIllustrationRuntimeIsJournalOptInAndDurable(t *testing.T) {
	platform := config.PlatformConfig{Environment: "development", StorageMode: "mysql"}
	app := config.AppConfig{Registry: appregistry.App{ID: appregistry.Journal}, Illustration: config.IllustrationConfig{BaseURL: "https://image.test/api/v3", APIKey: "synthetic", Model: "synthetic-model", MonthlyImages: 5}}
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	// An unopened database proves construction performs no SQL or dispatch.
	shared := sharedStorage{database: &sql.DB{}, cipher: cipher}
	controller, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 100, MaxConcurrent: 2, LeaseDuration: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	shared.imageCostController = controller
	app.Illustration.MaxCostNanos = 1
	storage := appStorage{keys: attestation.NewMemoryKeyStore(), entitlements: entitlement.NewMemoryStore(), privacy: privacy.NewMemoryRepository()}
	service, scheduler, err := buildIllustrationRuntime(platform, app, shared, storage)
	if err != nil || service != nil || scheduler != nil {
		t.Fatal("disabled feature constructed runtime")
	}
	app.Illustration.Enabled = true
	service, scheduler, err = buildIllustrationRuntime(platform, app, shared, storage)
	if err != nil || service == nil || scheduler == nil || service.Model != "synthetic-model" {
		t.Fatalf("runtime not composed: %v", err)
	}
	app.Registry.ID = appregistry.Health
	service, scheduler, err = buildIllustrationRuntime(platform, app, shared, storage)
	if err != nil || service != nil || scheduler != nil {
		t.Fatal("Health acquired image runtime")
	}
	app.Registry.ID = appregistry.Journal
	platform.StorageMode = "memory"
	if _, _, err = buildIllustrationRuntime(platform, app, shared, storage); err == nil {
		t.Fatal("volatile task storage accepted")
	}
	platform.StorageMode = "mysql"
	platform.Environment = "production"
	if _, _, err = buildIllustrationRuntime(platform, app, shared, storage); err != nil {
		t.Fatal("budgeted runtime rejected", err)
	}
	shared.imageCostController = nil
	if _, _, err = buildIllustrationRuntime(platform, app, shared, storage); err == nil {
		t.Fatal("unbudgeted runtime accepted")
	}
	shared.imageCostController = controller
	platform.Environment = "development"
	app.Illustration.BaseURL = "http://image.test"
	if _, _, err = buildIllustrationRuntime(platform, app, shared, storage); err == nil {
		t.Fatal("insecure endpoint accepted")
	}
	app.Illustration.BaseURL = "https://image.test"
	app.Illustration.MonthlyImages = 0
	if _, _, err = buildIllustrationRuntime(platform, app, shared, storage); err == nil {
		t.Fatal("unconfigured allowance accepted")
	}
}
