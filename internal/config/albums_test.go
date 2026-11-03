package config

import (
	"testing"

	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

func TestFourAppConfigurationDoesNotLoadAlbumAPISecretsIntoWorker(t *testing.T) {
	t.Setenv("PLATFORM_APPS", "health,journal,birthday,albums")
	t.Setenv("ALBUM_APPLE_TEAM_ID", "ALBUMTEAM")
	t.Setenv("ALBUM_APPLE_BUNDLE_ID", "")
	t.Setenv("ALBUM_API_HOST", "")
	t.Setenv("ALBUM_APPLE_SIGN_IN_KEY_FILE", "/must-not-be-read-by-platform-config")
	for _, environment := range []string{"development", "production"} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("APP_ENV", environment)
			platform, err := loadPlatformUnchecked()
			if err != nil {
				t.Fatal(err)
			}
			if len(platform.Apps) != 4 {
				t.Fatal("missing application")
			}
			album := platform.Apps[3]
			bundle, host := "cn.tellyouwhat.albums", "api.albums.tellyouwhat.cn"
			if environment == "development" {
				bundle += ".dev"
				host = "api-test.albums.tellyouwhat.cn"
			}
			if album.Registry.ID != appregistry.Albums || album.Registry.TeamID != "ALBUMTEAM" || album.Registry.BundleID != bundle || len(album.Registry.Hosts) != 1 || album.Registry.Hosts[0] != host {
				t.Fatal("album registry identity incorrect")
			}
			if err := album.Validate(environment); err != nil {
				t.Fatal(err)
			}
			if album.Registry.ManagedAIProductID != "" || len(album.ManagedAIProductIDs) != 0 || album.AppStore.PrivateKeyPath != "" {
				t.Fatal("album received AI plans or signing material")
			}
			opposite := "development"
			if environment == "development" {
				opposite = "production"
			}
			if err := album.Validate(opposite); err == nil {
				t.Fatal("cross-environment album accepted")
			}
		})
	}
	t.Setenv("PLATFORM_APPS", "")
	platform, err := loadPlatformUnchecked()
	if err != nil || len(platform.Apps) != 2 {
		t.Fatal("existing default deployment changed")
	}
}
