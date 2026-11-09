package albums

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRetentionDueIncludesMetadataOnlyAccounts(t *testing.T) {
	deadline := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	a := AlbumCloudAccount{State: "retention_due", RetentionDeadline: &deadline}
	if !retentionDue(a, deadline) {
		t.Fatal("zero media usage must not exclude retained collection metadata")
	}
	for _, state := range []string{"none", "active", "expired"} {
		a.State = state
		if retentionDue(a, deadline) {
			t.Fatalf("%s account authorized for expiry", state)
		}
	}
	a.State = "retention_due"
	if retentionDue(a, deadline.Add(time.Second)) {
		t.Fatal("different retained deadline accepted")
	}
	a.RetentionDeadline = nil
	if retentionDue(a, deadline) {
		t.Fatal("missing retained deadline accepted")
	}
}

func expiredCollectionFixture(t *testing.T) (sharingFixture, VerifiedAlbumSubscription, expiredRetentionProof) {
	t.Helper()
	f := newSharingFixture(t)
	var original, transaction string
	if err := f.db.QueryRow(`SELECT original_transaction_id,transaction_id FROM album_cloud_subscriptions WHERE owner_id=?`, f.login.AccountID).Scan(&original, &transaction); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(time.Second)
	v := VerifiedAlbumSubscription{original, transaction, Cloud50GBAnnualProduct, "Sandbox", f.now.AddDate(0, -7, 0), *f.now, false}
	applyReminderSubscription(t, f.repo, f.login.AccountID, v, *f.now)
	return f, v, verifyPurgeFixture(t, f.repo, f.login.AccountID, v, f.now)
}

func TestMySQLExpiredRetentionCollectionsZeroUsage(t *testing.T) {
	r, owner, now, v := reminderRepository(t)
	v.ExpiresAt = v.ExpiresAt.Truncate(time.Millisecond)
	if _, err := r.db.Exec(`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,SHA2(?,256))`, owner, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`UPDATE album_storage_accounts SET used_bytes=0 WHERE owner_id=?`, owner); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if _, err := r.db.Exec(`INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at) VALUES(?,?,1,UUID(),'synthetic',FALSE,?)`, owner, id, now); err != nil {
		t.Fatal(err)
	}
	applyReminderSubscription(t, r, owner, v, now)
	now = albumRetentionDeadline(v.ExpiresAt)
	proof := verifyPurgeFixture(t, r, owner, v, &now)
	if count, err := r.expireRetentionCollections(context.Background(), proof, 100, func() time.Time { return now }); err != nil || count != 1 {
		t.Fatal("metadata-only expiry", count, err)
	}
	var name string
	var deleted bool
	var revision int64
	if err := r.db.QueryRow(`SELECT name,deleted,revision FROM album_collections WHERE owner_id=? AND collection_id=?`, owner, id).Scan(&name, &deleted, &revision); err != nil || name != "" || !deleted || revision != 2 {
		t.Fatal("missing recovery tombstone", name, deleted, revision, err)
	}
	used, reserved := photoJournalBalance(t, r.db, owner)
	if used != 0 || reserved != 0 {
		t.Fatal("metadata expiry changed zero quota", used, reserved)
	}
}

func TestMySQLExpiredRetentionCollectionsBoundedReplayAndIsolation(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	owner, asset := f.login.AccountID, f.version.AssetID
	first, initial := createTestCollection(t, f)
	second, _ := createTestCollection(t, f)
	for _, id := range []string{first, second} {
		if _, err := f.repo.SaveCollectionMember(ctx, owner, id, asset, CloudCollectionMemberInput{uuid.NewString(), 0, true}, *f.now); err != nil {
			t.Fatal(err)
		}
	}
	oldDeleted, err := f.repo.SaveCollection(ctx, owner, second, CloudCollectionInput{uuid.NewString(), 1, "", true}, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	foreign := uuid.NewString()
	if _, err := f.db.Exec(`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,SHA2(?,256))`, foreign, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at) VALUES(?,?,1,UUID(),'other owner',FALSE,?)`, foreign, first, *f.now); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(time.Second)
	var original, transaction string
	if err := f.db.QueryRow(`SELECT original_transaction_id,transaction_id FROM album_cloud_subscriptions WHERE owner_id=?`, owner).Scan(&original, &transaction); err != nil {
		t.Fatal(err)
	}
	v := VerifiedAlbumSubscription{original, transaction, Cloud50GBAnnualProduct, "Sandbox", f.now.AddDate(0, -7, 0), *f.now, false}
	applyReminderSubscription(t, f.repo, owner, v, *f.now)
	proof := verifyPurgeFixture(t, f.repo, owner, v, f.now)
	used, reserved := photoJournalBalance(t, f.db, owner)
	for i := 0; i < 2; i++ {
		// Recreate the repository between batches to exercise persistent progress.
		r := NewMySQLUploads(f.db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return *f.now })
		if count, err := r.expireRetentionCollections(ctx, proof, 1, func() time.Time { return *f.now }); err != nil || count != 1 {
			t.Fatal("bounded expiry", count, err)
		}
	}
	if count, err := f.repo.expireRetentionCollections(ctx, proof, 1, func() time.Time { return *f.now }); err != nil || count != 0 {
		t.Fatal("replay duplicated work", count, err)
	}
	for _, id := range []string{first, second} {
		var name, request string
		var revision, sequence int64
		var deleted bool
		var at time.Time
		if err := f.db.QueryRow(`SELECT name,deleted,revision,sequence_id,request_id,updated_at FROM album_collections WHERE owner_id=? AND collection_id=?`, owner, id).Scan(&name, &deleted, &revision, &sequence, &request, &at); err != nil || name != "" || !deleted || revision != 2 || !validOwner(request) {
			t.Fatal("invalid tombstone", id, name, deleted, revision, err)
		}
		if id == first && (sequence <= initial.Sequence || !at.Equal(f.now.Truncate(time.Microsecond))) {
			t.Fatal("expiry lost incremental deletion event")
		}
		if id == second && (sequence != oldDeleted.Sequence || !at.Equal(oldDeleted.UpdatedAt)) {
			t.Fatal("expiry rewrote existing deletion evidence")
		}
	}
	var members int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_collection_members WHERE owner_id=?`, owner).Scan(&members); err != nil || members != 0 {
		t.Fatal("retired membership retained", members, err)
	}
	var foreignName string
	if err := f.db.QueryRow(`SELECT name FROM album_collections WHERE owner_id=? AND collection_id=?`, foreign, first).Scan(&foreignName); err != nil || foreignName != "other owner" {
		t.Fatal("foreign collection changed", foreignName, err)
	}
	version, err := f.repo.AssetVersion(ctx, owner, f.version.VersionID)
	if err != nil || !reflect.DeepEqual(version, f.version) {
		t.Fatal("metadata expiry changed original", err)
	}
	afterUsed, afterReserved := photoJournalBalance(t, f.db, owner)
	if afterUsed != used || afterReserved != reserved || f.signer.calls.Load() != 0 {
		t.Fatal("metadata expiry changed quota or issued media grants")
	}
}

func TestMySQLExpiredRetentionCollectionsRollbackAndFences(t *testing.T) {
	for _, fault := range []string{"insert", "expired-proof", "expires-during-write", "renewed", "quarantine", "environment", "revision", "bounded-sandbox", "development-grant", "advanced-server-clock"} {
		t.Run(fault, func(t *testing.T) {
			f, v, proof := expiredCollectionFixture(t)
			owner := f.login.AccountID
			// Seed metadata directly because public writes correctly reject expired accounts.
			seedCollections(t, f.db, owner)
			ctx := context.Background()
			var id string
			if err := f.db.QueryRow(`SELECT collection_id FROM album_collections WHERE owner_id=?`, owner).Scan(&id); err != nil {
				t.Fatal(err)
			}
			var before CloudCollection
			var beforeRequest string
			tx, err := f.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			before, beforeRequest, err = collectionRow(ctx, tx, owner, id)
			tx.Rollback()
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "insert":
				_, err = f.db.Exec(`ALTER TABLE album_collections ADD CONSTRAINT controlled_expiry_failure CHECK(deleted=FALSE)`)
			case "expired-proof":
				*f.now = proof.until
			case "renewed":
				renewPurgeFixture(t, f.repo, owner, v, *f.now)
			case "quarantine":
				_, err = f.db.Exec(`UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=? WHERE singleton_id=1`, *f.now)
			case "environment":
				proof.environment = "Production"
			case "revision":
				proof.revision++
			case "bounded-sandbox":
				f.repo.productionSandboxQuota = 1_000_000_000
			case "development-grant":
				f.repo.developmentCloud = &DevelopmentCloudConfig{}
			case "advanced-server-clock":
				f.repo.billingNow = func() time.Time { return proof.until }
			}
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			clock := func() time.Time {
				calls++
				if fault == "expires-during-write" && calls >= 2 {
					return proof.until
				}
				return *f.now
			}
			if count, err := f.repo.expireRetentionCollections(ctx, proof, 100, clock); err == nil || count != 0 {
				t.Fatal("unsafe expiry committed", count, err)
			}
			tx, err = f.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			after, request, err := collectionRow(ctx, tx, owner, id)
			tx.Rollback()
			if err != nil || !reflect.DeepEqual(after, before) || request != beforeRequest {
				t.Fatal("failed expiry changed group", after, err)
			}
			var members int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_collection_members WHERE owner_id=? AND collection_id=?`, owner, id).Scan(&members); err != nil || members != 1 {
				t.Fatal("failed expiry lost membership", members, err)
			}
		})
	}
}

func TestMySQLExpiredRetentionCollectionsRenewalAfterGrantDoesNotRestoreMetadata(t *testing.T) {
	f, v, proof := expiredCollectionFixture(t)
	owner := f.login.AccountID
	seedCollections(t, f.db, owner)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	held, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	expired := make(chan error, 1)
	go func() {
		first := true
		count, err := f.repo.expireRetentionCollections(ctx, proof, 100, func() time.Time {
			if first {
				first = false
				close(held)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return *f.now
		})
		if err == nil && count != 1 {
			err = ErrConflict
		}
		expired <- err
	}()
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal("expiry did not acquire owner lock")
	}
	started, renewed := make(chan struct{}), make(chan error, 1)
	go func() {
		v.Active, v.TransactionID, v.ExpiresAt, v.SignedAt = true, "renewed-after-metadata-grant", f.now.AddDate(1, 0, 0), f.now.Add(time.Millisecond)
		close(started)
		_, err := f.repo.ApplySubscription(ctx, owner, proof.revision, v, *f.now)
		renewed <- err
	}()
	<-started
	close(release)
	for _, result := range []<-chan error{expired, renewed} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	account, err := f.repo.CloudAccount(ctx, owner, *f.now)
	if err != nil || account.State != "active" {
		t.Fatal("renewal lost", account, err)
	}
	page, err := f.repo.Collections(ctx, owner, 0, 100, *f.now)
	if err != nil || len(page.Collections) != 1 || !page.Collections[0].Deleted || page.Collections[0].Name != "" {
		t.Fatal("renewal revived expired metadata", page, err)
	}
	if count, err := f.repo.expireRetentionCollections(ctx, proof, 100, func() time.Time { return *f.now }); !errors.Is(err, ErrLease) || count != 0 {
		t.Fatal("old proof survived renewal", count, err)
	}
}

func TestMySQLExpiredRetentionCollectionsBatchFailureRollsBackEarlierGroups(t *testing.T) {
	f, _, proof := expiredCollectionFixture(t)
	owner := f.login.AccountID
	seedCollections(t, f.db, owner)
	seedCollections(t, f.db, owner)
	var later string
	if err := f.db.QueryRow(`SELECT collection_id FROM album_collections WHERE owner_id=? ORDER BY sequence_id DESC LIMIT 1`, owner).Scan(&later); err != nil || !validOwner(later) {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`ALTER TABLE album_collections ADD CONSTRAINT controlled_later_expiry_failure CHECK(deleted=FALSE OR collection_id<>'` + later + `')`); err != nil {
		t.Fatal(err)
	}
	if count, err := f.repo.expireRetentionCollections(context.Background(), proof, 100, func() time.Time { return *f.now }); err == nil || count != 0 {
		t.Fatal("partial batch reported success", count, err)
	}
	var active, members int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_collections WHERE owner_id=? AND deleted=FALSE AND name<>'' AND revision=1`, owner).Scan(&active); err != nil || active != 2 {
		t.Fatal("earlier group escaped rollback", active, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_collection_members WHERE owner_id=?`, owner).Scan(&members); err != nil || members != 2 {
		t.Fatal("earlier membership escaped rollback", members, err)
	}
}

func TestExpiredRetentionCollectionsRejectsInvalidBatchBeforeSQL(t *testing.T) {
	r := &MySQLUploads{}
	for _, limit := range []int{-1, 0, 101} {
		if _, err := r.expireRetentionCollections(context.Background(), expiredRetentionProof{}, limit, time.Now); !errors.Is(err, ErrInvalidManifest) {
			t.Fatal(limit, err)
		}
	}
	if _, err := r.expireRetentionCollections(context.Background(), expiredRetentionProof{}, 1, nil); !errors.Is(err, ErrInvalidManifest) {
		t.Fatal(err)
	}
}
