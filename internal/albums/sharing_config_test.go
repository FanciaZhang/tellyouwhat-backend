package albums

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAlbumSharingConfigIsOptInPrivateAndIsolated(t *testing.T) {
	env := gatewayEnvironment(t)
	config, err := LoadGatewayConfig(func(k string) string { return env[k] })
	if err != nil || config.Sharing != nil {
		t.Fatal("sharing implicitly enabled", err)
	}
	key := bytes.Repeat([]byte{82}, 32)
	file := filepath.Join(t.TempDir(), "share-key")
	if err := os.WriteFile(file, []byte(base64.StdEncoding.EncodeToString(key)), 0600); err != nil {
		t.Fatal(err)
	}
	env["ALBUM_SHARING_ENABLED"] = "true"
	env["ALBUM_SHARE_SITE_ORIGIN"] = "https://shares.example"
	env["ALBUM_SHARE_KEY_FILE"] = file
	config, err = LoadGatewayConfig(func(k string) string { return env[k] })
	if err != nil || config.Sharing == nil {
		t.Fatal("valid private share config", err)
	}
	for _, text := range []string{fmt.Sprint(config), fmt.Sprintf("%#v", config), fmt.Sprint(config.Sharing), fmt.Sprintf("%#v", config.Sharing)} {
		if bytes.Contains([]byte(text), []byte(base64.StdEncoding.EncodeToString(key))) {
			t.Fatal("secret config logged")
		}
	}
	for _, origin := range []string{"http://shares.example", "https://shares.example/path", "https://user@shares.example", "https://shares.example:443", "https://shares.example#secret"} {
		env["ALBUM_SHARE_SITE_ORIGIN"] = origin
		if _, err := LoadGatewayConfig(func(k string) string { return env[k] }); err == nil {
			t.Fatal("unsafe share origin accepted")
		}
	}
	env["ALBUM_SHARE_SITE_ORIGIN"] = "https://shares.example"
	env["ALBUM_SHARE_PREVIOUS_KEY_FILE"] = file
	if _, err := LoadGatewayConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("duplicate keyring accepted")
	}
	delete(env, "ALBUM_SHARE_PREVIOUS_KEY_FILE")
	env["ALBUM_SHARE_KEY_FILE"] = env["ALBUM_LOGIN_LIMIT_KEY_FILE"]
	if _, err := LoadGatewayConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("privacy key reused for links")
	}
}
