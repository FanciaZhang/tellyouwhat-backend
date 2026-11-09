package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func TestMySQLProductionAndSandboxPurchasesKeepSeparateRightsAndRetention(t *testing.T) {
	db, _ := gatewayTestDatabase(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	production := NewMySQLUploads(db).WithSubscriptionEnvironment("Production", func() time.Time { return now })
	production.productionSandboxQuota = 1_000_000_000
	sandbox := *production
	sandbox.billingEnvironment = "Sandbox"
	owner := uuid.NewString()
	if err := production.ProvisionAccount(ctx, owner, 0); err != nil {
		t.Fatal(err)
	}
	trial := VerifiedAlbumSubscription{OriginalTransactionID: uuid.NewString(), TransactionID: uuid.NewString(), ProductID: Cloud500GBAnnualProduct, Environment: "Sandbox", ExpiresAt: now.Add(time.Minute), SignedAt: now, Active: true}
	a, err := sandbox.ApplySubscription(ctx, owner, 0, trial, now)
	if err != nil || a.State != "active" || a.QuotaBytes != 1_000_000_000 || a.RetentionDeadline != nil {
		t.Fatal("Sandbox effective capacity", a, err)
	}
	visible, err := production.CloudAccount(ctx, owner, now)
	if err != nil || visible.QuotaBytes != a.QuotaBytes || visible.RetentionDeadline != nil {
		t.Fatal("production reads did not expose bounded test entitlement", err)
	}
	if _, err := production.CloudMediaGrantDeadline(ctx, owner, now); err != nil {
		t.Fatal("test entitlement cannot upload", err)
	}
	now = now.Add(2 * time.Minute)
	trial.Active = false
	a, err = sandbox.ApplySubscription(ctx, owner, a.Revision, trial, now)
	if err != nil || a.State != "expired" || a.RetentionDeadline != nil || a.QuotaBytes != 0 {
		t.Fatal("accelerated expiry generated retention", a, err)
	}
	if _, err := production.CloudMediaGrantDeadline(ctx, owner, now); !errors.Is(err, ErrSubscriptionRequired) {
		t.Fatal("expired test entitlement still uploads", err)
	}
	paid := VerifiedAlbumSubscription{OriginalTransactionID: trial.OriginalTransactionID, TransactionID: uuid.NewString(), ProductID: Cloud50GBAnnualProduct, Environment: "Production", ExpiresAt: now.AddDate(1, 0, 0), SignedAt: now, Active: true}
	a, err = production.ApplySubscription(ctx, owner, a.Revision, paid, now)
	if err != nil || a.QuotaBytes != 50_000_000_000 || a.RetentionDeadline == nil {
		t.Fatal("paid upgrade", a, err)
	}
	deadline := *a.RetentionDeadline
	for _, active := range []bool{true, false} {
		now = now.Add(time.Second)
		trial.SignedAt, trial.Active = now, active
		trial.ExpiresAt = now.Add(time.Minute)
		if !active {
			trial.ExpiresAt = now.Add(-time.Second)
		}
		trial.TransactionID = uuid.NewString()
		a, err = sandbox.ApplySubscription(ctx, owner, a.Revision, trial, now)
		if err != nil || a.State != "active" || a.ProductID != Cloud50GBAnnualProduct || a.QuotaBytes != 50_000_000_000 || a.RetentionDeadline == nil || !a.RetentionDeadline.Equal(deadline) {
			t.Fatal("Sandbox overwrote paid rights", a, err)
		}
	}
	var chains, sandboxReminders int
	var projected int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_cloud_subscriptions WHERE owner_id=?`, owner).Scan(&chains); err != nil || chains != 2 {
		t.Fatal("environment chain collision", chains, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT quota_bytes FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&projected); err != nil || projected != 50_000_000_000 {
		t.Fatal("projection differs from effective rights", projected, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_retention_reminders WHERE owner_id=? AND environment='Sandbox'`, owner).Scan(&sandboxReminders); err != nil || sandboxReminders != 0 {
		t.Fatal("Sandbox created cleanup reminders", err)
	}
	// Freshness remains tied to the paid verification, not renewed by Sandbox.
	now = paid.SignedAt.Add(albumSubscriptionFreshness + time.Second)
	if _, err := production.CloudMediaGrantDeadline(ctx, owner, now); !errors.Is(err, ErrSubscriptionStale) {
		t.Fatal("Sandbox refreshed production authorization", err)
	}
}

func TestMySQLExpiredSandboxKeepsExistingOriginalsReadable(t *testing.T) {
	f := newSharingFixture(t)
	f.repo.productionSandboxQuota = 1_000_000_000
	f.repo.billingEnvironment = "Production"
	*f.now = f.now.AddDate(2, 0, 0)
	signer := &retainedReadSigner{AlbumObjects: f.uploads.objects}
	f.uploads.objects = signer
	before, err := f.repo.CloudAccount(context.Background(), f.login.AccountID, *f.now)
	if err != nil || before.State != "expired" || before.RetentionDeadline != nil || before.UsedBytes == 0 {
		t.Fatal("expired Sandbox fixture", before, err)
	}
	grant, err := f.uploads.OriginalReadGrant(context.Background(), f.login.AccountID, f.version.VersionID, "photo")
	if err != nil || !grant.ExpiresAt.Equal(f.now.Add(5*time.Minute)) || len(signer.expiries) != 1 {
		t.Fatal("Sandbox expiry blocked existing original download", err)
	}
	after, err := f.repo.CloudAccount(context.Background(), f.login.AccountID, *f.now)
	if err != nil || after.UsedBytes != before.UsedBytes || after.Revision != before.Revision {
		t.Fatal("download mutated existing media or entitlement", err)
	}
	if _, err := f.repo.CloudMediaGrantDeadline(context.Background(), f.login.AccountID, *f.now); !errors.Is(err, ErrSubscriptionRequired) {
		t.Fatal("download access reopened uploads", err)
	}
}
