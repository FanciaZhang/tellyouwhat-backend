package albums

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gatewayEnvironment(t *testing.T) map[string]string {
	t.Helper()
	values := workerEnvironment()
	values["ALBUM_DEPLOYMENT_ENVIRONMENT"] = "test"
	values["ALBUM_API_HOST"] = "api.albums.example"
	values["ALBUM_REDIS_URL"] = "redis://localhost:6379/0"
	values["ALBUM_APPLE_TEAM_ID"] = "TEAMID1234"
	values["ALBUM_APPLE_SIGN_IN_KEY_ID"] = "KEYID12345"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	credentialKey, privacyKey := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(credentialKey); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(privacyKey); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"ALBUM_APPLE_SIGN_IN_KEY_FILE": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		"ALBUM_CREDENTIAL_KEY_FILE":    []byte(base64.StdEncoding.EncodeToString(credentialKey) + "\n"),
		"ALBUM_LOGIN_LIMIT_KEY_FILE":   []byte(base64.StdEncoding.EncodeToString(privacyKey)),
	} {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		values[name] = path
	}
	return values
}

func TestGatewayConfigurationRejectsSharedStorageAndUnsafeCredentials(t *testing.T) {
	values := gatewayEnvironment(t)
	config, err := LoadGatewayConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenAddress != "127.0.0.1:8080" || len(config.TrustedProxies) != 0 || config.Worker.PhotoProcessing || config.Worker.VideoProcessing {
		t.Fatal("unsafe defaults")
	}
	for _, input := range [][2]string{
		{"ALBUM_DEPLOYMENT_ENVIRONMENT", ""}, {"ALBUM_DEPLOYMENT_ENVIRONMENT", "preview"},
		{"ALBUM_DATABASE_DSN", "app:private-db-secret@tcp(localhost:3306)/tellyouwhat"},
		{"ALBUM_DATABASE_DSN", "app:private-db-secret@tcp(localhost:3306)/health_test"},
		{"ALBUM_DATABASE_DSN", "root:private-db-secret@tcp(localhost:3306)/albums_test"},
		{"ALBUM_DATABASE_DSN", "not-a-dsn-private-db-secret"},
		{"ALBUM_API_HOST", "https://api.albums.example/"}, {"ALBUM_API_HOST", "api.albums.example:443"},
		{"ALBUM_API_HOST", "API.albums.example"}, {"ALBUM_API_HOST", "*.albums.example"},
		{"ALBUM_HTTP_LISTEN_ADDRESS", ":8080"}, {"ALBUM_HTTP_LISTEN_ADDRESS", "localhost:8080"},
		{"ALBUM_HTTP_LISTEN_ADDRESS", "0.0.0.0:0"},
		{"ALBUM_REDIS_URL", "redis://private-redis-secret@localhost:6379/1"},
		{"ALBUM_REDIS_URL", "https://private-redis-secret.invalid"},
		{"ALBUM_TRUSTED_PROXIES", "0.0.0.0/0"}, {"ALBUM_TRUSTED_PROXIES", "203.0.113.0/24"},
		{"ALBUM_TRUSTED_PROXIES", "172.29.90.1/24"},
		{"ALBUM_APPLE_SIGN_IN_KEY_ID", "private-invalid-key-id"},
		{"ALBUM_APPLE_SIGN_IN_KEY_FILE", "/missing-private-key-file"},
		{"ALBUM_CREDENTIAL_KEY_FILE", "relative-private-secret-file"},
		{"ALBUM_LOGIN_LIMIT_KEY_FILE", values["ALBUM_CREDENTIAL_KEY_FILE"]},
	} {
		t.Run(input[0]+"/"+fmt.Sprint(len(input[1])), func(t *testing.T) {
			copy := make(map[string]string, len(values))
			for k, v := range values {
				copy[k] = v
			}
			copy[input[0]] = input[1]
			_, err := LoadGatewayConfig(func(k string) string { return copy[k] })
			if err == nil {
				t.Fatal("unsafe deployment accepted")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("configuration error disclosed a secret")
			}
		})
	}
	for _, productionDB := range []string{"albums", "albums_production"} {
		values["ALBUM_DEPLOYMENT_ENVIRONMENT"] = "production"
		values["ALBUM_DATABASE_DSN"] = "app:private-secret@tcp(localhost:3306)/" + productionDB
		values["ALBUM_TRUSTED_PROXIES"] = "127.0.0.1/32,172.29.90.1/32"
		if _, err := LoadGatewayConfig(func(k string) string { return values[k] }); err != nil {
			t.Fatal("independent production database rejected", err)
		}
	}
	if output := fmt.Sprintf("%v %+v %#v", config, config, config); strings.Contains(output, "private-") || strings.Contains(output, config.CredentialKey) || strings.Contains(output, string(config.AppleCode.PrivateKeyPEM)) {
		t.Fatal("gateway configuration formatting disclosed credentials")
	}
}

func TestGatewaySecretFilesRejectPublicPermissionsAndMalformedKeys(t *testing.T) {
	for _, mode := range []os.FileMode{0644, 0660, 0700} {
		t.Run(mode.String(), func(t *testing.T) {
			values := gatewayEnvironment(t)
			if err := os.Chmod(values["ALBUM_CREDENTIAL_KEY_FILE"], mode); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadGatewayConfig(func(k string) string { return values[k] }); err == nil {
				t.Fatal("unsafe key file accepted")
			}
		})
	}
	for _, value := range [][]byte{[]byte("not-base64"), []byte(base64.StdEncoding.EncodeToString(make([]byte, 31))), bytes.Repeat([]byte{'x'}, 129)} {
		values := gatewayEnvironment(t)
		if err := os.WriteFile(values["ALBUM_LOGIN_LIMIT_KEY_FILE"], value, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadGatewayConfig(func(k string) string { return values[k] }); err == nil {
			t.Fatal("malformed key accepted")
		}
	}
	values := gatewayEnvironment(t)
	if err := os.Chmod(values["ALBUM_CREDENTIAL_KEY_FILE"], 0440); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGatewayConfig(func(k string) string { return values[k] }); err != nil {
		t.Fatal("container group-readable key rejected", err)
	}
	if _, err := LoadGatewayConfig(nil); err == nil {
		t.Fatal("missing environment reader accepted")
	}
}
