package main

import (
	"errors"

	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

// Construct without dispatching work. The caller starts the scheduler only
// after all routers have been built and stops it before closing storage.
func buildIllustrationRuntime(platform config.PlatformConfig, app config.AppConfig, shared sharedStorage, storage appStorage) (*illustration.Service, *illustration.Scheduler, error) {
	if app.Registry.ID != appregistry.Journal || !app.Illustration.Enabled {
		return nil, nil, nil
	}
	if platform.Environment == "production" {
		return nil, nil, errors.New("image production enablement requires project monetary cost control integration")
	}
	if platform.StorageMode != "mysql" || shared.database == nil || shared.cipher == nil || storage.keys == nil || storage.entitlements == nil {
		return nil, nil, errors.New("image generation requires durable encrypted Journal storage")
	}
	consents, ok := storage.privacy.(privacy.ConsentReader)
	if !ok || app.Illustration.MonthlyImages < 1 {
		return nil, nil, errors.New("image generation requires consent storage and a positive allowance")
	}
	provider, err := illustration.NewProvider(app.Illustration.BaseURL, app.Illustration.APIKey, app.Illustration.Model, nil)
	if err != nil {
		return nil, nil, errors.New("invalid Journal image provider configuration")
	}
	policy := &illustration.AccessPolicy{Keys: storage.keys, Subscriptions: storage.entitlements, Consents: consents, MonthlyLimit: int64(app.Illustration.MonthlyImages)}
	repository := mysqlstore.NewIllustrationRepository(shared.database, shared.cipher, "journal", policy)
	policy.Reservations = repository
	service := &illustration.Service{Store: repository, Access: policy, Model: provider.Model()}
	worker := illustration.Worker{Store: repository, Guard: policy, Generator: provider}
	scheduler := &illustration.Scheduler{Queue: repository, Processor: worker}
	return service, scheduler, nil
}
