package albums

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

func retentionPurgeFixture(t *testing.T) (*MySQLUploads, Upload, *time.Time, VerifiedAlbumSubscription, expiredRetentionProof) {
	t.Helper()
	db, u, now := purgeFixture(t)
	setDerivedAssetTrash(t, db, u, false, now)
	if _, err := db.Exec(`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,SHA2(?,256))`, u.OwnerID, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	now = now.Truncate(time.Millisecond)
	r := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return now })
	v := VerifiedAlbumSubscription{"retention-chain", "retention-transaction", Cloud50GBAnnualProduct, "Sandbox", now.AddDate(0, -7, 0), now.Add(-time.Minute), false}
	applyReminderSubscription(t, r, u.OwnerID, v, now)
	proof := verifyPurgeFixture(t, r, u.OwnerID, v, &now)
	return r, u, &now, v, proof
}

func verifyPurgeFixture(t *testing.T, r *MySQLUploads, owner string, v VerifiedAlbumSubscription, clock *time.Time) expiredRetentionProof {
	t.Helper()
	f := newAlbumReceiptFixture(t, *clock)
	client := reminderStatusFunc(func(context.Context, string) ([]appstore.SubscriptionStatus, error) {
		return []appstore.SubscriptionStatus{reminderSignedStatus(t, f, owner, v)}, nil
	})
	service := reminderVerificationService(t, r, f, client, func() time.Time { return *clock })
	proof, err := service.verifyExpiredRetention(context.Background(), r, owner)
	if err != nil {
		t.Fatal("fresh expiry proof", err)
	}
	return proof
}

func renewPurgeFixture(t *testing.T, r *MySQLUploads, owner string, v VerifiedAlbumSubscription, now time.Time) {
	t.Helper()
	v.Active, v.TransactionID, v.ExpiresAt, v.SignedAt = true, "renewed-retention", now.AddDate(1, 0, 0), now.Add(time.Millisecond)
	applyReminderSubscription(t, r, owner, v, now)
}

func TestMySQLRetentionPurgeRenewalDuringInventoryCancels(t *testing.T) {
	r, u, clock, v, proof := retentionPurgeFixture(t)
	ctx := context.Background()
	p, err := r.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock)
	if err != nil {
		t.Fatal(err)
	}
	s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID, versions: map[string][]DerivedObjectVersion{p.Targets[0].Key: {{"keep", false, 4}}}}
	s.beforeList = func() { renewPurgeFixture(t, r, u.OwnerID, v, *clock) }
	if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return *clock }); !errors.Is(err, ErrNotFound) {
		t.Fatal("renewal did not fence inventory", err)
	}
	if len(s.deletes) != 0 || len(s.aborts) != 0 {
		t.Fatal("provider mutation after renewal")
	}
	if _, err := r.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock); !errors.Is(err, ErrLease) {
		t.Fatal("old proof reused", err)
	}
	if state, err := r.CloudAsset(ctx, u.OwnerID, u.Manifest.AssetID, *clock); err != nil || state.State != "active" {
		t.Fatal("renewal lost accessible media", state, err)
	}
}

func TestMySQLRetentionPurgeFirstActionRequiresLiveProofAndAtomicGrant(t *testing.T) {
	for _, fault := range []string{"renewal", "proof-expired", "revision-changed", "grant-write", "expires-during-grant"} {
		t.Run(fault, func(t *testing.T) {
			r, u, clock, v, proof := retentionPurgeFixture(t)
			ctx := context.Background()
			if _, err := r.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock); err != nil {
				t.Fatal(err)
			}
			p, err := r.claimObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, uuid.NewString(), *clock)
			if err != nil {
				t.Fatal(err)
			}
			p, err = r.storeObjectPurgePlan(ctx, p, purgePlan{Versions: []DerivedObjectVersion{{"keep", false, 4}}}, *clock)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "renewal":
				renewPurgeFixture(t, r, u.OwnerID, v, *clock)
			case "proof-expired":
				*clock = proof.until
			case "revision-changed":
				v.SignedAt = *clock
				applyReminderSubscription(t, r, u.OwnerID, v, *clock)
			case "grant-write":
				if _, err := r.db.Exec(`ALTER TABLE album_library_changes ADD CONSTRAINT block_purge_grant CHECK(state<>'purged')`); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			grantClock := func() time.Time {
				calls++
				if fault == "expires-during-grant" && calls >= 3 {
					return proof.until
				}
				return *clock
			}
			if _, err := r.authorizeObjectPurgeAction(ctx, p, grantClock); err == nil {
				t.Fatal("invalid first action authorized")
			}
			var state string
			if err := r.db.QueryRow(`SELECT state FROM album_asset_lifecycle WHERE owner_id=? AND asset_id=?`, u.OwnerID, u.Manifest.AssetID).Scan(&state); err != nil || state != "active" {
				t.Fatal("failed grant changed visibility", state, err)
			}
			current, err := r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
			if fault == "renewal" {
				if !errors.Is(err, ErrNotFound) {
					t.Fatal("queued purge survived renewal", err)
				}
				return
			}
			if err != nil || !current.StartedAt.IsZero() {
				t.Fatal("failed grant marked started", current, err)
			}
			if fault == "proof-expired" {
				fresh := verifyPurgeFixture(t, r, u.OwnerID, v, clock)
				if _, err := r.prepareRetentionObjectPurge(ctx, fresh, u.Manifest.AssetID, *clock); err != nil {
					t.Fatal("fresh proof cannot replace stale plan", err)
				}
				if _, err := r.authorizeObjectPurgeAction(ctx, p, func() time.Time { return *clock }); !errors.Is(err, ErrLease) {
					t.Fatal("old plan survived replacement", err)
				}
			}
		})
	}
}

func TestMySQLRetentionPurgeStartedDeletionSurvivesRenewalAndRestart(t *testing.T) {
	r, u, clock, v, proof := retentionPurgeFixture(t)
	ctx := context.Background()
	p, err := r.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock)
	if err != nil {
		t.Fatal(err)
	}
	s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID, loseOnce: true, versions: map[string][]DerivedObjectVersion{p.Targets[0].Key: {{"lost-reply", false, 4}, {"next", false, 4}}}}
	// Exercise SQL microsecond precision with a normal nanosecond Go clock.
	*clock = clock.Add(123 * time.Nanosecond)
	now := func() time.Time { return *clock }
	if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, now); !errors.Is(err, ErrCOSOperation) {
		t.Fatal(err)
	}
	p, err = r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
	if err != nil || p.StartedAt.IsZero() || p.ActionIndex != 0 {
		t.Fatal("lost reply lost grant", p, err)
	}
	renewPurgeFixture(t, r, u.OwnerID, v, *clock)
	if _, err := r.CloudAsset(ctx, u.OwnerID, u.Manifest.AssetID, *clock); !errors.Is(err, ErrNotFound) {
		t.Fatal("partly erased backup visible", err)
	}
	if _, err := r.VisibleAssetVersion(ctx, u.OwnerID, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("original download still grantable", err)
	}
	if err := r.ValidateUploadVisibility(ctx, u); !errors.Is(err, ErrNotFound) {
		t.Fatal("old backup receipt reusable", err)
	}
	page, err := r.LibraryChanges(ctx, u.OwnerID, 0, 100)
	if err != nil || len(page.Changes) != 1 || page.Changes[0].State != "purged" || page.Changes[0].Version != nil {
		t.Fatal("backup invalidation missing", page, err)
	}
	if _, err := r.MutateCloudAsset(ctx, u.OwnerID, u.Manifest.AssetID, "trash", CloudAssetMutation{uuid.NewString(), p.Revision}, *clock); !errors.Is(err, ErrConflict) {
		t.Fatal("purging media mutable", err)
	}
	*clock = clock.Add(11 * time.Minute)
	r = NewMySQLUploads(r.db).WithSubscriptionEnvironment("Sandbox", now)
	s.r = r
	for range len(p.Targets) + 1 {
		if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, now); err != nil {
			t.Fatal("resume after renewal", err)
		}
	}
	if !reflect.DeepEqual(s.deletes, []string{"lost-reply", "lost-reply", "next"}) {
		t.Fatal("retry changed exact plan", s.deletes)
	}
	for range 2 {
		refund, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, now)
		if err != nil || refund != u.OriginalBytes {
			t.Fatal("settlement replay", refund, err)
		}
	}
	used, reserved := photoJournalBalance(t, r.db, u.OwnerID)
	if used != 0 || reserved != 0 {
		t.Fatal("refund incorrect", used, reserved)
	}
}

func TestMySQLRetentionPurgeEmptyInventorySettlementStillChecksRenewal(t *testing.T) {
	for _, renewBefore := range []bool{false, true} {
		t.Run(map[bool]string{false: "after-settlement", true: "before-settlement"}[renewBefore], func(t *testing.T) {
			r, u, clock, v, proof := retentionPurgeFixture(t)
			ctx := context.Background()
			p, err := r.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock)
			if err != nil {
				t.Fatal(err)
			}
			s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID}
			now := func() time.Time { return *clock }
			for range len(p.Targets) {
				if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, now); err != nil {
					t.Fatal(err)
				}
			}
			if renewBefore {
				renewPurgeFixture(t, r, u.OwnerID, v, *clock)
			}
			refund, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, now)
			if renewBefore {
				if !errors.Is(err, ErrNotFound) || refund != 0 {
					t.Fatal("renewal failed to cancel empty plan", refund, err)
				}
				used, _ := photoJournalBalance(t, r.db, u.OwnerID)
				if used != u.OriginalBytes {
					t.Fatal("cancelled cleanup refunded")
				}
				return
			}
			if err != nil || refund != u.OriginalBytes {
				t.Fatal(refund, err)
			}
			renewPurgeFixture(t, r, u.OwnerID, v, *clock)
			replay, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, now)
			if err != nil || replay != refund {
				t.Fatal("renewal erased completed settlement", replay, err)
			}
		})
	}
}

func TestMySQLRetentionPurgeCannotReplaceExpiredTrash(t *testing.T) {
	r, u, clock, v, proof := retentionPurgeFixture(t)
	ctx := context.Background()
	setDerivedAssetTrash(t, r.db, u, true, clock.Add(-30*24*time.Hour))
	p, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, *clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock); !errors.Is(err, ErrConflict) {
		t.Fatal("replaced independent trash", err)
	}
	renewPurgeFixture(t, r, u.OwnerID, v, *clock)
	after, err := r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
	if err != nil || !reflect.DeepEqual(p, after) {
		t.Fatal("renewal cancelled independent trash", err)
	}
}

func TestMySQLRetentionPurgeFirstActionRechecksAfterAccountLock(t *testing.T) {
	r, u, clock, v, proof := retentionPurgeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock); err != nil {
		t.Fatal(err)
	}
	p, err := r.claimObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, uuid.NewString(), *clock)
	if err != nil {
		t.Fatal(err)
	}
	p, err = r.storeObjectPurgePlan(ctx, p, purgePlan{Versions: []DerivedObjectVersion{{"keep", false, 4}}}, *clock)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	v.Active, v.TransactionID, v.ExpiresAt, v.SignedAt = true, "renewed-under-lock", clock.AddDate(1, 0, 0), clock.Add(time.Millisecond)
	if _, err := r.applySubscriptionInTransaction(ctx, tx, u.OwnerID, proof.revision, v, *clock); err != nil {
		t.Fatal(err)
	}
	began, done := make(chan struct{}), make(chan error, 1)
	go func() {
		first := true
		_, err := r.authorizeObjectPurgeAction(ctx, p, func() time.Time {
			if first {
				first = false
				close(began)
			}
			return *clock
		})
		done <- err
	}()
	select {
	case <-began:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotFound) {
			t.Fatal("waiting worker ignored committed renewal", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := r.VisibleAssetVersion(ctx, u.OwnerID, u.ID); err != nil {
		t.Fatal("renewal failed to preserve media", err)
	}
}
