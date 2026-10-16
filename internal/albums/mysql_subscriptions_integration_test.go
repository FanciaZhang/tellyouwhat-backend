package albums

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

// Runs on a generated schema and a restricted MySQL user, never a shared
// development or production database. Decisions below stand for verified JWS;
// signature/account-token verification has separate certificate-chain tests.
func TestMySQLAlbumSubscriptionsAdmissionDowngradeAndRetention(t *testing.T) {
	db, _ := gatewayTestDatabase(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal("migration replay", err)
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 123456000, time.UTC)
	r := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return now })
	if err := r.CheckSubscriptionSchema(ctx); err != nil {
		t.Fatal(err)
	}
	owner := uuid.NewString()
	// A legacy projected limit cannot bypass the runtime subscription gate.
	if err := r.ProvisionAccount(ctx, owner, 500_000_000_000); err != nil {
		t.Fatal(err)
	}
	_, _, objects, _, manifest := uploadFixture(t)
	s, err := NewUploadService(r, objects, UploadPolicy{1 << 30, 1 << 20, 4 * time.Hour, time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, owner, uuid.NewString(), manifest); !errors.Is(err, ErrSubscriptionRequired) {
		t.Fatal("unverified capacity admitted upload", err)
	}
	decision := VerifiedAlbumSubscription{"chain-1", "transaction-1", Cloud50GBAnnualProduct, "Sandbox", now.Add(time.Hour), now, true}
	a, err := r.ApplySubscription(ctx, owner, 0, decision, now)
	if err != nil || a.QuotaBytes != 50_000_000_000 || a.Revision != 1 {
		t.Fatal("50 GB admission", a, err)
	}
	u, err := s.Create(ctx, owner, uuid.NewString(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := s.Grants(ctx, owner, u.ID)
	if err != nil || len(grants) != 1 {
		t.Fatal("paid grant", err)
	}
	// Already uploaded content can finish after expiry; only new writes stop.
	objects.staging[stagingKey(u, "photo")] = []byte("original-resource-bytes")
	now = decision.ExpiresAt.Add(-10 * time.Second)
	a, err = r.CloudAccount(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	// Current verified status refreshes the write authorization window; a
	// one-hour-old cached entitlement alone is deliberately insufficient.
	a, err = r.ApplySubscription(ctx, owner, a.Revision, decision, now)
	if err != nil {
		t.Fatal(err)
	}
	grants, err = s.Grants(ctx, owner, u.ID)
	if err != nil || len(grants) != 1 || !grants[0].ExpiresAt.Equal(decision.ExpiresAt) {
		t.Fatal("PUT outlived subscription", grants, err)
	}
	now = decision.ExpiresAt.Add(time.Second)
	if _, err := s.Grants(ctx, owner, u.ID); !errors.Is(err, ErrSubscriptionRequired) {
		t.Fatal("expired subscription issued PUT", err)
	}
	newManifest := manifest
	newManifest.SourceRevision = "new-revision-after-expiry"
	if _, err := s.Create(ctx, owner, uuid.NewString(), newManifest); !errors.Is(err, ErrSubscriptionRequired) {
		t.Fatal("expiry admitted new upload", err)
	}
	if _, err := s.Submit(ctx, owner, u.ID); err != nil {
		t.Fatal("expiry prevented existing upload submission", err)
	}
	if _, err := s.Verify(ctx, owner, u.ID); err != nil {
		t.Fatal("expiry prevented verified backup completion", err)
	}
	a, err = r.CloudAccount(ctx, owner, now)
	if err != nil || a.State != "expired" || a.UsedBytes != manifest.Resources[0].SizeBytes || a.ReservedBytes != u.DerivedBudget || !a.RetentionDeadline.Equal(time.Date(2027, 2, 28, 13, 0, 0, 123456000, time.UTC)) {
		t.Fatal("expiry lost data or wrong deadline", a, err)
	}
	deadline := *a.RetentionDeadline
	decision.Active = false
	decision.SignedAt = now
	a, err = r.ApplySubscription(ctx, owner, a.Revision, decision, now)
	if err != nil || !a.RetentionDeadline.Equal(deadline) {
		t.Fatal("inactive evidence extended retention", err)
	}
	now = deadline
	a, err = r.CloudAccount(ctx, owner, now)
	if err != nil || a.State != "retention_due" || a.UsedBytes == 0 {
		t.Fatal("deadline projection or early deletion", a, err)
	}
	production := NewMySQLUploads(db).WithSubscriptionEnvironment("Production", func() time.Time { return now })
	if a, err := production.CloudAccount(ctx, owner, now); err != nil || a.State != "none" || a.QuotaBytes != 0 {
		t.Fatal("Sandbox capacity crossed into production", a, err)
	}
	if _, err := production.ApplySubscription(ctx, owner, a.Revision, decision, now); !errors.Is(err, ErrSubscriptionVerification) {
		t.Fatal("production accepted Sandbox decision", err)
	}

	// Two independent chains select the greatest current tier, never a sum.
	owner = uuid.NewString()
	if err := r.ProvisionAccount(ctx, owner, 0); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	large := VerifiedAlbumSubscription{"chain-500", "txn-500", Cloud500GBAnnualProduct, "Sandbox", now.Add(5 * time.Minute), now, true}
	small := VerifiedAlbumSubscription{"chain-50", "txn-50", Cloud50GBAnnualProduct, "Sandbox", now.Add(time.Hour), now, true}
	a, err = r.ApplySubscription(ctx, owner, 0, large, now)
	if err != nil {
		t.Fatal(err)
	}
	a, err = r.ApplySubscription(ctx, owner, a.Revision, small, now)
	if err != nil || a.QuotaBytes != 500_000_000_000 {
		t.Fatal("tiers were added", a, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=60000000000 WHERE owner_id=?`, owner); err != nil {
		t.Fatal(err)
	}
	now = large.ExpiresAt
	a, err = r.CloudAccount(ctx, owner, now)
	if err != nil || a.State != "active" || a.QuotaBytes != 50_000_000_000 || a.UsedBytes != 60_000_000_000 || a.AvailableBytes != 0 {
		t.Fatal("scheduled downgrade lost usage", a, err)
	}
	if _, err := s.Create(ctx, owner, uuid.NewString(), manifest); !errors.Is(err, ErrQuota) {
		t.Fatal("stale 500 GB projection admitted an upload", err)
	}
	if _, err := r.CloudMediaGrantDeadline(ctx, owner, now); !errors.Is(err, ErrQuota) {
		t.Fatal("over-quota downgrade granted media write", err)
	}
	// A late external verification cannot overwrite newer state.
	if _, err := r.ApplySubscription(ctx, owner, a.Revision-1, small, now); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatal("stale account revision accepted", err)
	}
	older := small
	older.SignedAt = small.SignedAt.Add(-time.Second)
	if _, err := r.ApplySubscription(ctx, owner, a.Revision, older, now); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatal("older chain evidence accepted", err)
	}
}

func TestMySQLAlbumSubscriptionChainCanBelongToOnlyOneAccount(t *testing.T) {
	db, _ := gatewayTestDatabase(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	r := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return now })
	owners := []string{uuid.NewString(), uuid.NewString()}
	for _, owner := range owners {
		if err := r.ProvisionAccount(ctx, owner, 0); err != nil {
			t.Fatal(err)
		}
	}
	v := VerifiedAlbumSubscription{"same-chain", "same-transaction", Cloud50GBAnnualProduct, "Sandbox", now.Add(time.Hour), now, true}
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, owner := range owners {
		group.Add(1)
		go func(owner string) {
			defer group.Done()
			<-start
			_, err := r.ApplySubscription(ctx, owner, 0, v, now)
			results <- err
		}(owner)
	}
	close(start)
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("competing chain claims did not produce one owner", successes)
	}
	var bound string
	if err := db.QueryRowContext(ctx, `SELECT owner_id FROM album_cloud_subscriptions WHERE original_transaction_id='same-chain'`).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	for _, owner := range owners {
		a, err := r.CloudAccount(ctx, owner, now)
		if err != nil {
			t.Fatal(err)
		}
		if owner == bound {
			if a.QuotaBytes != 50_000_000_000 {
				t.Fatal("winner lost capacity")
			}
			continue
		}
		if a.QuotaBytes != 0 {
			t.Fatal("loser gained capacity")
		}
		if _, err := r.ApplySubscription(ctx, owner, a.Revision, v, now); !errors.Is(err, ErrSubscriptionBinding) {
			t.Fatal("chain reassigned after race", err)
		}
	}
}
