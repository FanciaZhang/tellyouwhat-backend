package main

import (
	"errors"

	"github.com/tellyouwhat/backend/internal/albums"
	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

func validateAlbumGatewayIdentity(platform config.PlatformConfig, app appregistry.App, album albums.GatewayConfig) error {
	environment := "production"
	if platform.Environment == "development" {
		environment = "test"
	}
	if app.ID != appregistry.Albums || album.Environment != environment || len(app.Hosts) != 1 ||
		app.Hosts[0] != album.APIHost || app.TeamID != album.AppleCode.TeamID || app.BundleID != album.AppleCode.BundleID {
		return errors.New("album API identity must match the shared gateway environment and registry")
	}
	return nil
}
