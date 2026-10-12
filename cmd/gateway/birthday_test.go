package main

import (
	"testing"

	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/storage/redisstore"
)

func TestBirthdayQuotaDoesNotRequireOtherAppsPublishedPolicy(t *testing.T) {
	for _, id := range []appregistry.AppID{appregistry.Birthday, appregistry.Health} {
		storage := storageForApp(config.PlatformConfig{StorageMode: "mysql"}, sharedStorage{}, config.AppConfig{Registry: appregistry.App{ID: id}})
		limiter, ok := storage.limiter.(*redisstore.QuotaLimiter)
		if !ok || (limiter.ResolveLimits == nil) != (id == appregistry.Birthday) {
			t.Fatalf("%s quota policy isolation is incorrect", id)
		}
	}
}
