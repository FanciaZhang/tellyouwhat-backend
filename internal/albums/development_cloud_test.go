package albums

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func developmentCloudEnvironment(owner string) map[string]string {
	return map[string]string{
		"ALBUM_DEPLOYMENT_ENVIRONMENT": "test", "ALBUM_APPLE_BUNDLE_ID": albumDevelopmentAudience,
		"ALBUM_DATABASE_DSN":                  "albums_test:private-test-password@tcp(localhost:3306)/albums_test",
		"ALBUM_DEVELOPMENT_CLOUD_QUOTA_BYTES": "1000000000", "ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS": owner,
		"ALBUM_DEVELOPMENT_CLOUD_EXPIRES_AT": "2026-11-15T00:00:00Z",
	}
}

func TestDevelopmentCloudConfigRequiresExplicitBoundedTestGrant(t *testing.T) {
	if c, err := loadDevelopmentCloudConfig(func(string) string { return "" }); err != nil || c != nil {
		t.Fatal("development capacity was implicitly enabled")
	}
	owner := uuid.NewString()
	values := developmentCloudEnvironment(owner)
	c, err := loadDevelopmentCloudConfig(func(k string) string { return values[k] })
	if err != nil || c == nil || c.quota != 1_000_000_000 || len(c.owners) != 1 {
		t.Fatal("valid explicit grant rejected", err)
	}
	values["ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS"] = owner + ", " + uuid.NewString() + ", " + uuid.NewString()
	if c, err := loadDevelopmentCloudConfig(func(k string) string { return values[k] }); err != nil || len(c.owners) != 3 {
		t.Fatal("three explicit accounts rejected", err)
	}
	for _, input := range [][2]string{
		{"ALBUM_DEPLOYMENT_ENVIRONMENT", "production"}, {"ALBUM_DEPLOYMENT_ENVIRONMENT", ""},
		{"ALBUM_APPLE_BUNDLE_ID", albumAppleAudience}, {"ALBUM_SUBSCRIPTIONS_ENABLED", "true"},
		{"ALBUM_DATABASE_DSN", "root:private-test-password@tcp(localhost:3306)/albums_test"},
		{"ALBUM_DATABASE_DSN", "albums:private-test-password@tcp(localhost:3306)/albums"},
		{"ALBUM_DEVELOPMENT_CLOUD_QUOTA_BYTES", "1000000001"}, {"ALBUM_DEVELOPMENT_CLOUD_QUOTA_BYTES", "0"},
		{"ALBUM_DEVELOPMENT_CLOUD_QUOTA_BYTES", ""}, {"ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS", "*"},
		{"ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS", ""}, {"ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS", owner + "," + owner},
		{"ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS", uuid.NewString() + "," + uuid.NewString() + "," + uuid.NewString() + "," + uuid.NewString()},
		{"ALBUM_DEVELOPMENT_CLOUD_EXPIRES_AT", ""}, {"ALBUM_DEVELOPMENT_CLOUD_EXPIRES_AT", "invalid"},
	} {
		t.Run(input[0]+"="+strings.Split(input[1], ":")[0], func(t *testing.T) {
			v := developmentCloudEnvironment(owner)
			v[input[0]] = input[1]
			if _, err := loadDevelopmentCloudConfig(func(k string) string { return v[k] }); err == nil || strings.Contains(err.Error(), "private-test-password") {
				t.Fatal("unsafe grant accepted or credentials exposed", err)
			}
		})
	}
}

func TestDevelopmentCloudGrantIsNotAPurchaseAndNeverAuthorizesExpiryDeletion(t *testing.T) {
	owner := uuid.NewString()
	values := developmentCloudEnvironment(owner)
	c, err := loadDevelopmentCloudConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{-time.Hour, 0, time.Hour} {
		now := c.expiresAt.Add(offset)
		a := AlbumCloudAccount{AccountID: owner, State: "none", UsedBytes: 100}
		c.apply(&a, now)
		a, err = cloudAccountBalance(a)
		if err != nil || a.QuotaKind != "development" || a.ProductID != "" || a.PurchasesEnabled || a.RetentionDeadline != nil || retentionDue(a, now) {
			t.Fatal("grant invented a purchase or deletion deadline", a, err)
		}
		deadline, permission := cloudAccountMediaDeadline(a)
		if offset < 0 {
			if a.State != "active" || a.AvailableBytes != c.quota-100 || permission != nil || deadline.After(now.Add(5*time.Minute)) {
				t.Fatal("active grant did not enforce a bounded upload lease", a, permission)
			}
		} else if a.State != "expired" || a.QuotaBytes != 0 || !errors.Is(permission, ErrSubscriptionRequired) {
			t.Fatal("expired grant still authorizes uploads", a, permission)
		}
	}
	a := AlbumCloudAccount{AccountID: uuid.NewString(), State: "none"}
	c.apply(&a, c.expiresAt.Add(-time.Hour))
	if a.State != "none" || a.QuotaBytes != 0 || a.ExpiresAt != nil {
		t.Fatal("unlisted account received capacity")
	}
	r := NewMySQLUploads(nil).WithDevelopmentCloud(c)
	if err := r.checkExpiredRetention(context.Background(), nil, expiredRetentionProof{}, time.Now()); !errors.Is(err, ErrLease) {
		t.Fatal("development grant accepted a deletion capability", err)
	}
}

func TestDevelopmentCloudCannotEnterAProductionGateway(t *testing.T) {
	c := GatewayConfig{Environment: "production", Worker: WorkerConfig{DevelopmentCloud: &DevelopmentCloudConfig{}}}
	c.AppleCode.BundleID = albumAppleAudience
	if _, err := NewGateway(context.Background(), c, nil); err == nil {
		t.Fatal("production accepted development capacity before opening storage")
	}
}

func TestGatewayAndWorkerLoadTheSameDevelopmentCloudGrant(t *testing.T) {
	values := gatewayEnvironment(t)
	owner := uuid.NewString()
	for k, v := range developmentCloudEnvironment(owner) {
		values[k] = v
	}
	worker, err := LoadWorkerConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := LoadGatewayConfig(func(k string) string { return values[k] })
	if err != nil || worker.DevelopmentCloud == nil || gateway.Worker.DevelopmentCloud == nil || gateway.Subscriptions != nil {
		t.Fatal("runtime grant wiring failed", err)
	}
	if worker.DevelopmentCloud.quota != gateway.Worker.DevelopmentCloud.quota ||
		!worker.DevelopmentCloud.expiresAt.Equal(gateway.Worker.DevelopmentCloud.expiresAt) {
		t.Fatal("gateway and worker resolved different grants")
	}
}
