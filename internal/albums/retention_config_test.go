package albums

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAlbumRetentionPushConfigFailClosed(t *testing.T) {
	values := gatewayEnvironment(t)
	gateway, err := LoadGatewayConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	if gateway.RetentionAPNS != nil {
		t.Fatal("sending enabled by default")
	}
	fixture, _, _, _ := apnsFixture(t)
	keyPath := filepath.Join(t.TempDir(), "push.p8")
	if err := os.WriteFile(keyPath, fixture.PrivateKeyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	gateway.Subscriptions = &AlbumSubscriptionConfig{KeyID: "STOREKEY01"}
	gateway.PushEnvironment = "production"
	values["ALBUM_RETENTION_PUSH_ENABLED"] = "true"
	values["ALBUM_APNS_KEY_ID"] = fixture.KeyID
	values["ALBUM_APNS_KEY_FILE"] = keyPath
	for _, name := range []string{"valid", "missing-subscriptions", "missing-registration", "invalid-flag", "reuse-login-key", "reuse-store-key", "missing-file", "public-file", "invalid-key", "invalid-environment"} {
		t.Run(name, func(t *testing.T) {
			c := gateway
			env := map[string]string{}
			for k, v := range values {
				env[k] = v
			}
			switch name {
			case "missing-subscriptions":
				c.Subscriptions = nil
			case "missing-registration":
				c.PushEnvironment = ""
			case "invalid-flag":
				env["ALBUM_RETENTION_PUSH_ENABLED"] = "yes"
			case "reuse-login-key":
				env["ALBUM_APNS_KEY_ID"] = c.AppleCode.KeyID
			case "reuse-store-key":
				env["ALBUM_APNS_KEY_ID"] = c.Subscriptions.KeyID
			case "missing-file":
				env["ALBUM_APNS_KEY_FILE"] = "/private-missing-fixture"
			case "public-file":
				path := filepath.Join(t.TempDir(), "public")
				if err := os.WriteFile(path, fixture.PrivateKeyPEM, 0644); err != nil {
					t.Fatal(err)
				}
				env["ALBUM_APNS_KEY_FILE"] = path
			case "invalid-key":
				path := filepath.Join(t.TempDir(), "invalid")
				if err := os.WriteFile(path, []byte("private-invalid-key"), 0600); err != nil {
					t.Fatal(err)
				}
				env["ALBUM_APNS_KEY_FILE"] = path
			case "invalid-environment":
				c.PushEnvironment = "Sandbox"
			}
			config, err := loadRetentionAPNSConfig(func(k string) string { return env[k] }, c)
			if name == "valid" {
				if err != nil || config == nil || config.TeamID != gateway.AppleCode.TeamID {
					t.Fatal("valid config", err)
				}
			} else if err == nil || config != nil {
				t.Fatal("unsafe config enabled")
			}
			if err != nil && strings.Contains(err.Error(), "private-") {
				t.Fatal("secret disclosed")
			}
		})
	}
}
func TestGatewayRetentionDispatcherAssemblyAndDrain(t *testing.T) {
	f := newPushFixture(t)
	values := gatewayEnvironment(t)
	values["ALBUM_DATABASE_DSN"] = f.dsn
	values["ALBUM_REDIS_URL"] = os.Getenv("REDIS_TEST_URL")
	values["ALBUM_PUSH_ENVIRONMENT"] = "production"
	receipt := newAlbumReceiptFixture(t, time.Now().UTC())
	key := albumTestECKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"ALBUM_APP_STORE_KEY_FILE": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), "ALBUM_APP_STORE_ROOTS_FILE": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: receipt.rootDER})} {
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		values[name] = path
	}
	values["ALBUM_SUBSCRIPTIONS_ENABLED"] = "true"
	values["ALBUM_APP_STORE_ISSUER_ID"] = uuid.NewString()
	values["ALBUM_APP_STORE_KEY_ID"] = "STOREKEY01"
	apns, _, _, _ := apnsFixture(t)
	path := filepath.Join(t.TempDir(), "push.p8")
	if err := os.WriteFile(path, apns.PrivateKeyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	values["ALBUM_RETENTION_PUSH_ENABLED"] = "true"
	values["ALBUM_APNS_KEY_ID"] = apns.KeyID
	values["ALBUM_APNS_KEY_FILE"] = path
	config, err := LoadGatewayConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	transport := appleTestTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if !r.URL.Query().Has("versioning") {
			t.Error("empty database should not call Apple")
		}
		return cosResponse(r, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, ""), nil
	})
	gateway, err := NewGateway(context.Background(), config, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	if gateway.retentionCancel == nil || gateway.subscriptionCancel == nil {
		t.Fatal("runtime worker missing")
	}
	gateway.BeginDrain()
	done := make(chan struct{})
	go func() { gateway.subscriptionWorkers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("workers did not stop on drain")
	}
	if requests.Load() != 1 {
		t.Fatal("unexpected provider calls", requests.Load())
	}
}
