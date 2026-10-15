package albums

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAlbumSubscriptionConfigurationIsExplicitAndEnvironmentBound(t *testing.T) {
	fixture := newAlbumReceiptFixture(t, time.Now().UTC())
	keyDER, err := x509.MarshalPKCS8PrivateKey(albumTestECKey(t))
	if err != nil {
		t.Fatal(err)
	}
	privateFile := func(name string, data []byte) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	keyFile := privateFile("album.p8", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	rootsFile := privateFile("test-root.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.rootDER}))
	baseline := map[string]string{"ALBUM_SUBSCRIPTIONS_ENABLED": "true", "ALBUM_APP_STORE_ISSUER_ID": uuid.NewString(), "ALBUM_APP_STORE_KEY_ID": "ALBUMSKEY1", "ALBUM_APP_STORE_KEY_FILE": keyFile, "ALBUM_APP_STORE_ROOTS_FILE": rootsFile}
	for _, name := range []string{"disabled", "erasure", "erasure-invalid", "erasure-without-rebinding", "erasure-without-verification", "rebinding", "rebinding-invalid", "rebinding-without-verification", "test", "production", "production-id-missing", "flag-invalid", "issuer-invalid", "key-id-invalid", "key-unavailable", "key-oversized", "roots-invalid", "invalid-deployment"} {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{}
			for k, v := range baseline {
				values[k] = v
			}
			deployment, wantEnvironment, rejected := "test", "Sandbox", false
			switch name {
			case "erasure":
				values["ALBUM_SUBSCRIPTION_REBINDING_ENABLED"] = "true"
				values["ALBUM_ACCOUNT_RECORD_ERASURE_ENABLED"] = "true"
			case "erasure-invalid":
				values["ALBUM_ACCOUNT_RECORD_ERASURE_ENABLED"] = "yes"
				rejected = true
			case "erasure-without-rebinding":
				values["ALBUM_ACCOUNT_RECORD_ERASURE_ENABLED"] = "true"
				rejected = true
			case "erasure-without-verification":
				values["ALBUM_ACCOUNT_RECORD_ERASURE_ENABLED"] = "true"
				values["ALBUM_SUBSCRIPTION_REBINDING_ENABLED"] = "true"
				values["ALBUM_SUBSCRIPTIONS_ENABLED"] = "false"
				rejected = true
			case "rebinding":
				values["ALBUM_SUBSCRIPTION_REBINDING_ENABLED"] = "true"
			case "rebinding-invalid":
				values["ALBUM_SUBSCRIPTION_REBINDING_ENABLED"] = "yes"
				rejected = true
			case "rebinding-without-verification":
				values["ALBUM_SUBSCRIPTIONS_ENABLED"] = "false"
				values["ALBUM_SUBSCRIPTION_REBINDING_ENABLED"] = "true"
				rejected = true
			case "disabled":
				values["ALBUM_SUBSCRIPTIONS_ENABLED"] = "false"
				values["ALBUM_APP_STORE_KEY_FILE"] = "missing"
			case "production":
				deployment = "production"
				wantEnvironment = "Production"
				values["ALBUM_APP_STORE_APP_APPLE_ID"] = "12345"
			case "production-id-missing":
				deployment = "production"
				rejected = true
			case "flag-invalid":
				values["ALBUM_SUBSCRIPTIONS_ENABLED"] = "yes"
				rejected = true
			case "issuer-invalid":
				values["ALBUM_APP_STORE_ISSUER_ID"] = "Apple-team-is-not-an-issuer"
				rejected = true
			case "key-id-invalid":
				values["ALBUM_APP_STORE_KEY_ID"] = "short"
				rejected = true
			case "key-unavailable":
				values["ALBUM_APP_STORE_KEY_FILE"] = "relative.p8"
				rejected = true
			case "key-oversized":
				values["ALBUM_APP_STORE_KEY_FILE"] = privateFile("large.p8", []byte(strings.Repeat("x", (16<<10)+1)))
				rejected = true
			case "roots-invalid":
				values["ALBUM_APP_STORE_ROOTS_FILE"] = privateFile("invalid.pem", []byte("not certificates"))
				rejected = true
			case "invalid-deployment":
				deployment = "preview"
				rejected = true
			}
			config, err := loadAlbumSubscriptionConfig(func(k string) string { return values[k] }, deployment)
			if rejected {
				if err == nil || config != nil {
					t.Fatal("unsafe configuration accepted")
				}
				return
			}
			if name == "disabled" {
				if err != nil || config != nil {
					t.Fatal("disabled billing read keys", err)
				}
				return
			}
			if err != nil || config == nil || config.RebindingEnabled != (name == "rebinding" || name == "erasure") || config.RecordErasureEnabled != (name == "erasure") || config.Environment != wantEnvironment || config.BundleID != albumAudienceForDeployment(deployment) || config.SigningKey == nil || config.Roots == nil {
				t.Fatal("realm selection", err)
			}
			if value := fmt.Sprintf("%v %#v", *config, *config); strings.Contains(value, config.IssuerID) || strings.Contains(value, config.KeyID) || !strings.Contains(value, "redacted") {
				t.Fatal("config logging disclosed identities")
			}
		})
	}
}
