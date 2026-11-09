package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

func TestDevelopmentCommerceUsesAppleVerificationWithoutActivator(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, appID := range []appregistry.AppID{appregistry.Health, appregistry.Journal} {
		app := config.AppConfig{}
		app.Registry.ID = appID
		app.Registry.BundleID = "cn.tellyouwhat." + string(appID) + ".dev"
		app.ManagedAIProductIDs = []string{"fixture.subscription.dev"}
		app.AppStore = config.AppStoreConfig{Environment: "Sandbox", RootPEMPath: "../../deploy/apple-app-store-roots.pem", PrivateKeyPath: path, KeyID: "FIXTUREKEY", IssuerID: "fixture"}
		activation, sync, notification, err := commerceServices("development", app, nil, nil)
		if err != nil || activation != nil || sync == nil || notification == nil {
			t.Fatalf("%s does not use verified Sandbox commerce: %v", appID, err)
		}
		app.AppStore.Environment = "Both"
		if _, _, _, err = commerceServices("development", app, nil, nil); err == nil {
			t.Fatal("development accepted production transactions")
		}
	}
}
