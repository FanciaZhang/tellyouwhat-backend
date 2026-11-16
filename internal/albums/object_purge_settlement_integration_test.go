package albums

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/migrations"
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
	seedSystemCopyHints(t, db, u.OwnerID)
	seedSystemCopyHints(t, db, other.OwnerID)
	seedContentCorrections(t, db, u.OwnerID)
	seedContentCorrections(t, db, other.OwnerID)
	seedAssetFavorites(t, db, u.OwnerID)
	seedCollections(t, db, u.OwnerID)
	seedAssetFavorites(t, db, other.OwnerID)
	seedCollections(t, db, other.OwnerID)
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
	var ownHints, foreignHints int
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_system_copy_locators WHERE owner_id=?`, u.OwnerID).Scan(&ownHints); err != nil || ownHints != 0 {
		t.Fatal("purged hints retained", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_system_copy_locators WHERE owner_id=?`, other.OwnerID).Scan(&foreignHints); err != nil || foreignHints != 1 {
		t.Fatal("foreign hints removed", err)
	}
	for owner, expected := range map[string]int{u.OwnerID: 0, other.OwnerID: 1} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM album_content_corrections WHERE owner_id=?`, owner).Scan(&count); err != nil || count != expected {
			t.Fatal("correction cascade or owner isolation", count, expected, err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM album_collection_members WHERE owner_id=?`, owner).Scan(&count); err != nil || count != expected {
			t.Fatal("membership purge or owner isolation", count, expected, err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM album_collections WHERE owner_id=?`, owner).Scan(&count); err != nil || count != 1 {
			t.Fatal("asset purge deleted collection", count, err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM album_asset_favorites WHERE owner_id=?`, owner).Scan(&count); err != nil || count != expected {
			t.Fatal("favorite erasure or owner isolation", count, expected, err)
		}
	}
	p, err := r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
	if err != nil || p.State != "completed" || len(p.Targets) != 0 {
		t.Fatal("object keys retained after settlement", p, err)
	}
	var revision int64
	var reason string
	var purgedAt time.Time
	var data []byte
	if err := db.QueryRow(`SELECT final_revision,reason,purged_at,version_ids FROM album_asset_deletion_ledger WHERE owner_id=? AND asset_id=?`, u.OwnerID, u.Manifest.AssetID).Scan(&revision, &reason, &purgedAt, &data); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if json.Unmarshal(data, &ids) != nil || len(ids) != 1 || ids[0] != u.ID || revision != 3 || reason != "trash" || !purgedAt.Equal(now.Truncate(time.Microsecond)) {
		t.Fatal("missing exact deletion fence", revision, reason, purgedAt, string(data))
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
	var receipts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_asset_deletion_ledger`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("rolled back settlement left a fence", receipts, err)
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

func TestMySQLAssetDeletionLedgerFailureMigrationAndReplay(t *testing.T) {
	db, u, now := purgeFixture(t)
	ctx := context.Background()
	r := NewMySQLUploads(db)
	s := drainPurgeFixture(t, r, u, now)
	if _, err := db.Exec(`ALTER TABLE album_asset_deletion_ledger ADD CONSTRAINT block_asset_receipt CHECK(owner_id='never')`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); err == nil {
		t.Fatal("settled without receipt")
	}
	if _, err := r.AssetVersion(ctx, u.OwnerID, u.ID); err != nil {
		t.Fatal("receipt failure removed metadata", err)
	}
	used, _ := photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes {
		t.Fatal("receipt failure refunded quota")
	}
	if _, err := db.Exec(`ALTER TABLE album_asset_deletion_ledger DROP CHECK block_asset_receipt`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	// Simulate an older completed purge, and DDL success before migration bookkeeping.
	if _, err := db.Exec(`DELETE FROM album_asset_deletion_ledger`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); err == nil {
		t.Fatal("completed receipt silently accepted missing fence")
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name='0078_album_asset_deletion_ledger.sql'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Run(ctx, db); err != nil {
			t.Fatal("backfill or repeat", err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_asset_deletion_ledger WHERE owner_id=? AND asset_id=? AND final_revision=3 AND reason='trash' AND JSON_LENGTH(version_ids)=1 AND JSON_UNQUOTE(JSON_EXTRACT(version_ids,'$[0]'))=?`, u.OwnerID, u.Manifest.AssetID, u.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("migration did not preserve exact fence", count, err)
	}
	if _, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); err != nil {
		t.Fatal("backfilled replay", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.key_column_usage WHERE table_schema=DATABASE() AND table_name='album_asset_deletion_ledger' AND referenced_table_name IS NOT NULL`).Scan(&count); err != nil || count != 0 {
		t.Fatal("fence depends on live account", count, err)
	}
}

func TestGatewayAssetDeletionLedgerMissingSchemaRejectsStartup(t *testing.T) {
	f := newAppleGatewayFixture(t)
	if _, err := f.db.Exec(`DROP TABLE album_asset_deletion_ledger`); err != nil {
		t.Fatal(err)
	}
	g, err := NewGateway(context.Background(), f.config, nil)
	if g != nil {
		g.Close()
	}
	if err == nil {
		t.Fatal("missing asset fence schema accepted")
	}
}
