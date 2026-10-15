package main

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/storage/redisstore"
	"github.com/tellyouwhat/backend/internal/testutil/appattest"
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

func TestBirthdayAutomaticEnrollmentStillVerifiesAppleIdentityAndBuild(t *testing.T) {
	for _, tc := range []struct {
		name, identity, realm, build string
		valid                        bool
	}{
		{"approved development app", "TEAM.cn.tellyouwhat.birthday.dev", "production", "4", true},
		{"formal app", "TEAM.cn.tellyouwhat.birthday", "production", "4", false},
		{"other team", "OTHER.cn.tellyouwhat.birthday.dev", "production", "4", false},
		{"wrong proof realm", "TEAM.cn.tellyouwhat.birthday.dev", "development", "4", false},
		{"unapproved build", "TEAM.cn.tellyouwhat.birthday.dev", "production", "5", false},
		{"missing proof", "TEAM.cn.tellyouwhat.birthday.dev", "", "4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, ctx := time.Now().UTC(), context.Background()
			client := appattest.New(t, tc.identity, now)
			app := config.AppConfig{Registry: appregistry.App{ID: appregistry.Birthday}}
			keys := attestation.NewMemoryKeyStore()
			service := attestation.NewEnrollmentService(attestation.EnrollmentConfig{
				AppID: "birthday", Environment: attestation.EnvironmentProduction,
				RequireActivationSecret: app.RequiresActivationSecret("development"),
				AllowedBuilds:           map[string]struct{}{"4": {}},
			}, attestation.NewMemoryNonceStore(), keys,
				attestation.NewAppleAttestationVerifier("TEAM", "cn.tellyouwhat.birthday.dev", attestation.EnvironmentProduction, client.Roots),
				func() time.Time { return now })
			challenge, err := service.IssueChallenge(ctx, client.KeyID)
			if err != nil {
				t.Fatal(err)
			}
			request := attestation.RegistrationRequest{KeyID: client.KeyID, Challenge: challenge.Value, Build: tc.build}
			if tc.realm != "" {
				request.Attestation = base64.StdEncoding.EncodeToString(client.Attestation(t, challenge.Value, tc.realm))
			}
			principal, err := service.Register(ctx, request)
			stored, lookupErr := keys.Get(ctx, client.KeyID)
			if tc.valid {
				if err != nil || lookupErr != nil || principal.AppID != "birthday" || stored.DeviceID != principal.DeviceID {
					t.Fatalf("automatic verified enrollment failed: %v / %v", err, lookupErr)
				}
			} else if !errors.Is(err, attestation.ErrEnrollmentDenied) || !errors.Is(lookupErr, attestation.ErrKeyNotFound) {
				t.Fatalf("invalid identity/build enrolled: %v / %v", err, lookupErr)
			}
		})
	}
}
