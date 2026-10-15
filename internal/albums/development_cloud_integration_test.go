package albums

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func developmentStorageFixture(t *testing.T, quota int64) (*pushFixture, AccountLogin, *DevelopmentCloudConfig) {
	t.Helper()
	f := newPushFixture(t)
	_, account := f.login(t, "development-cloud-owner", f.now)
	values := developmentCloudEnvironment(account.AccountID)
	values["ALBUM_DATABASE_DSN"] = f.dsn
	values["ALBUM_DEVELOPMENT_CLOUD_QUOTA_BYTES"] = strconv.FormatInt(quota, 10)
	values["ALBUM_DEVELOPMENT_CLOUD_EXPIRES_AT"] = f.now.Add(time.Hour).Format(time.RFC3339)
	c, err := loadDevelopmentCloudConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	return f, account, c
}

func TestMySQLDevelopmentCloudGatewayWorkerQuotaAndExpiredExport(t *testing.T) {
	f, account, config := developmentStorageFixture(t, 4096)
	ctx := context.Background()
	clock := func() time.Time { return f.now }
	api := NewMySQLUploads(f.r.db).WithDevelopmentCloud(config).WithSubscriptionEnvironment("Sandbox", clock)
	worker := NewMySQLUploads(f.r.db).WithDevelopmentCloud(config).WithSubscriptionEnvironment("Sandbox", clock)
	_, _, objects, _, manifest := uploadFixture(t)
	manifest.AssetID = uuid.NewString()
	manifest.Resources[0].Descriptor = &ResourceDescriptor{PhotoKitType: 1, Filename: "fixture.jpeg", UniformTypeIdentifier: "public.jpeg"}
	signer := &retainedReadSigner{AlbumObjects: objects}
	service, err := NewUploadService(api, signer, UploadPolicy{1024, 1024, time.Hour, time.Minute}, clock)
	if err != nil {
		t.Fatal(err)
	}
	upload, err := service.Create(ctx, account.AccountID, uuid.NewString(), manifest)
	if err != nil {
		t.Fatal("explicit grant did not reserve without StoreKit", err)
	}
	if _, err = service.Grants(ctx, account.AccountID, upload.ID); err != nil {
		t.Fatal(err)
	}
	objects.staging[stagingKey(upload, "photo")] = []byte("original-resource-bytes")
	if _, err = service.Submit(ctx, account.AccountID, upload.ID); err != nil {
		t.Fatal(err)
	}
	processor, err := NewUploadService(worker, objects, UploadPolicy{1024, 1024, time.Hour, time.Minute}, clock)
	if err != nil {
		t.Fatal(err)
	}
	if verified, err := processor.Verify(ctx, account.AccountID, upload.ID); err != nil || verified.State != OriginalsVerified {
		t.Fatal("worker did not honor the same explicit grant", verified.State, err)
	}
	var rawQuota, purchases int64
	if err = f.r.db.QueryRow(`SELECT quota_bytes FROM album_storage_accounts WHERE owner_id=?`, account.AccountID).Scan(&rawQuota); err != nil {
		t.Fatal(err)
	}
	if err = f.r.db.QueryRow(`SELECT COUNT(*) FROM album_cloud_subscriptions WHERE owner_id=?`, account.AccountID).Scan(&purchases); err != nil {
		t.Fatal(err)
	}
	if rawQuota != 0 || purchases != 0 {
		t.Fatal("test access fabricated a persisted purchase or paid capacity")
	}
	version, err := api.AssetVersion(ctx, account.AccountID, upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.now = config.expiresAt
	state, err := api.CloudAccount(ctx, account.AccountID, f.now)
	if err != nil || state.State != "expired" || state.UsedBytes == 0 || state.RetentionDeadline != nil {
		t.Fatal(state, err)
	}
	manifest.AssetID = uuid.NewString()
	if _, err = service.Create(ctx, account.AccountID, uuid.NewString(), manifest); !errors.Is(err, ErrSubscriptionRequired) {
		t.Fatal("expired grant admitted new media", err)
	}
	grant, err := service.OriginalReadGrant(ctx, account.AccountID, version.VersionID, "photo")
	if err != nil || !grant.ExpiresAt.Equal(f.now.Add(5*time.Minute)) {
		t.Fatal("expired owner lost bounded original export", err)
	}
	if _, err = service.OriginalReadGrant(ctx, uuid.NewString(), version.VersionID, "photo"); err == nil {
		t.Fatal("foreign account reached original export")
	}
	if err := api.checkExpiredRetention(ctx, nil, expiredRetentionProof{owner: account.AccountID}, f.now); !errors.Is(err, ErrLease) {
		t.Fatal("development expiry accepted an automatic deletion capability", err)
	}
}

func TestMySQLDevelopmentCloudReservationSerializesQuotaAndRejectsUnlistedOwner(t *testing.T) {
	f, account, config := developmentStorageFixture(t, 69)
	ctx := context.Background()
	r := NewMySQLUploads(f.r.db).WithDevelopmentCloud(config).WithSubscriptionEnvironment("Sandbox", func() time.Time { return f.now })
	_, _, objects, _, template := uploadFixture(t)
	service, _ := NewUploadService(r, objects, UploadPolicy{1024, 1024, time.Hour, time.Minute}, func() time.Time { return f.now })
	var group sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			manifest := template
			manifest.AssetID = uuid.NewString()
			_, err := service.Create(ctx, account.AccountID, uuid.NewString(), manifest)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	accepted, refused := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if errors.Is(err, ErrQuota) {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatal("concurrent uploads exceeded the test grant", accepted, refused)
	}
	_, other := f.login(t, "unlisted-cloud-owner", f.now)
	if _, err := service.Create(ctx, other.AccountID, uuid.NewString(), template); !errors.Is(err, ErrSubscriptionRequired) {
		t.Fatal("unlisted owner received cloud capacity", err)
	}
}
