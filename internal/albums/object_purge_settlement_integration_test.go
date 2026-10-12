package albums

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func drainPurgeFixture(t *testing.T, r *MySQLUploads, u Upload, now time.Time) *purgeStorageFixture {
	t.Helper()
	ctx := context.Background()
	p, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now)
	if err != nil {
		t.Fatal(err)
	}
	s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID, versions: map[string][]DerivedObjectVersion{}}
	for range len(p.Targets) {
		if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestMySQLObjectPurgeSettlementAtomicRefundAndFinalFeed(t *testing.T) {
	db, u, now := purgeFixture(t)
	ctx := context.Background()
	r := NewMySQLUploads(db)
	other, _ := photoJournalUpload(t, db)
	old, err := r.LibraryChanges(ctx, u.OwnerID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	s := drainPurgeFixture(t, r, u, now)
	var group sync.WaitGroup
	for range 6 {
		group.Go(func() {
			refund, err := NewMySQLUploads(db).settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now })
			if err != nil || refund != u.OriginalBytes {
				t.Error("refund not idempotent", refund, err)
			}
		})
	}
	group.Wait()
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != 0 || reserved != 0 {
		t.Fatal("incorrect balance", used, reserved)
	}
	if _, err := r.AssetVersion(ctx, u.OwnerID, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("metadata retained", err)
	}
	if _, err := r.AssetVersion(ctx, other.OwnerID, other.ID); err != nil {
		t.Fatal("foreign media deleted", err)
	}
	for _, after := range []int64{0, old.NextSequence} {
		page, err := r.LibraryChanges(ctx, u.OwnerID, after, 1)
		if err != nil || len(page.Changes) != 1 || page.HasMore || page.Changes[0].State != "purged" || page.Changes[0].Version != nil || page.Changes[0].TrashedAt != nil || page.Changes[0].Revision != 3 {
			t.Fatal("fresh or old cursor lost tombstone", page, err)
		}
	}
	p, err := r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
	if err != nil || p.State != "completed" || len(p.Targets) != 0 {
		t.Fatal("object keys retained after settlement", p, err)
	}
	u.ID = u.RequestID
	if _, err := r.Reserve(ctx, u); err == nil {
		t.Fatal("old asset identity resurrected")
	}
}

func TestMySQLObjectPurgeSettlementRequiresFreshAbsenceAndRollsBack(t *testing.T) {
	db, u, now := purgeFixture(t)
	ctx := context.Background()
	r := NewMySQLUploads(db)
	s := drainPurgeFixture(t, r, u, now)
	p, _ := r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
	s.versions[p.Targets[0].Key] = []DerivedObjectVersion{{"late", false, 3}}
	if _, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("nonempty key settled", err)
	}
	delete(s.versions, p.Targets[0].Key)
	s = drainPurgeFixture(t, r, u, now)
	// Force a failure after deletion of child rows and old feed events. The whole
	// transaction must preserve quota, source manifests and the previous feed.
	if _, err := db.Exec(`CREATE TABLE settlement_block(owner_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,version_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin,FOREIGN KEY(owner_id,version_id) REFERENCES album_asset_versions(owner_id,version_id))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settlement_block VALUES(?,?)`, u.OwnerID, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); err == nil {
		t.Fatal("late DB failure accepted")
	}
	used, _ := photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes {
		t.Fatal("failed settlement changed quota", used)
	}
	if _, err := r.AssetVersion(ctx, u.OwnerID, u.ID); err != nil {
		t.Fatal("failed transaction lost original metadata", err)
	}
	page, err := r.LibraryChanges(ctx, u.OwnerID, 0, 100)
	if err != nil || len(page.Changes) != 1 || page.Changes[0].State != "active" {
		t.Fatal("partial feed commit", page, err)
	}
	p, _ = r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
	if p.State != "objects_removed" {
		t.Fatal("failed settlement marked completed")
	}
	if _, err := db.Exec(`DROP TABLE settlement_block`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE album_storage_accounts SET used_bytes=0 WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); !errors.Is(err, ErrQuota) {
		t.Fatal("underflow accepted", err)
	}
	if _, err := db.Exec(`UPDATE album_storage_accounts SET used_bytes=? WHERE owner_id=?`, u.OriginalBytes, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); err != nil {
		t.Fatal("could not retry", err)
	}
}
