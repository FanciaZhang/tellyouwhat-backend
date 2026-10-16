package main

import (
	"testing"

	"github.com/tellyouwhat/backend/internal/albums"
	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

func TestAlbumGatewayRejectsCrossEnvironmentAndIdentityBeforeConnecting(t *testing.T) {
	for _, environment := range []string{"development", "production"} {
		t.Run(environment, func(t *testing.T) {
			platform := config.PlatformConfig{Environment: environment}
			album := albums.GatewayConfig{Environment: "production", APIHost: "api.albums.tellyouwhat.cn",
				AppleCode: albums.AppleCodeConfig{TeamID: "TEAM", BundleID: "cn.tellyouwhat.albums"}}
			if environment == "development" {
				album.Environment = "test"
				album.APIHost = "api-test.albums.tellyouwhat.cn"
				album.AppleCode.BundleID += ".dev"
			}
			app := appregistry.App{ID: appregistry.Albums, Hosts: []string{album.APIHost},
				TeamID: album.AppleCode.TeamID, BundleID: album.AppleCode.BundleID}
			if err := validateAlbumGatewayIdentity(platform, app, album); err != nil {
				t.Fatal(err)
			}
			for _, mutate := range []func(*albums.GatewayConfig){
				func(c *albums.GatewayConfig) { c.Environment = "wrong" },
				func(c *albums.GatewayConfig) { c.APIHost = "other.test" },
				func(c *albums.GatewayConfig) { c.AppleCode.TeamID = "OTHER" },
				func(c *albums.GatewayConfig) { c.AppleCode.BundleID = "other.bundle" },
			} {
				bad := album
				mutate(&bad)
				if validateAlbumGatewayIdentity(platform, app, bad) == nil {
					t.Fatal("conflicting album configuration accepted")
				}
			}
			app.Hosts = append(app.Hosts, "extra.test")
			if validateAlbumGatewayIdentity(platform, app, album) == nil {
				t.Fatal("unvalidated alternate album host accepted")
			}
		})
	}
}
