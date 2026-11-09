package albums

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func originalSealFixture(t *testing.T) (*MySQLUploads, *UploadService, *objectTestStore, Upload, *time.Time) {
	t.Helper()
	db, _ := gatewayTestDatabase(t)
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r := NewMySQLUploads(db)
	_, _, o, owner, m := uploadFixture(t)
	clock := time.Now().UTC().Truncate(time.Microsecond)
	if err := r.ProvisionAccount(context.Background(), owner, 1<<20); err != nil {
		t.Fatal(err)
	}
	s, err := NewUploadService(r, o, UploadPolicy{1 << 20, 1 << 20, time.Hour, time.Minute}, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Create(context.Background(), owner, uuid.NewString(), m)
	if err != nil {
		t.Fatal(err)
	}
	o.staging[stagingKey(u, "photo")] = []byte("original-resource-bytes")
	if _, err := s.Submit(context.Background(), owner, u.ID); err != nil {
		t.Fatal(err)
	}
	return r, s, o, u, &clock
}

func TestMySQLOriginalSealGrantSerializesAndSurvivesLeaseReplacement(t *testing.T) {
	r, _, _, u, clock := originalSealFixture(t)
	ctx := context.Background()
	claimed, err := r.Claim(ctx, u.OwnerID, u.ID, uuid.NewString(), *clock, clock.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var fresh atomic.Int32
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			_, ok, err := r.BeginOriginalSeal(ctx, claimed, "photo", *clock)
			if err != nil {
				t.Error(err)
			}
			if ok {
				fresh.Add(1)
			}
		})
	}
	group.Wait()
	if fresh.Load() != 1 {
		t.Fatal("multiple COPY grants", fresh.Load())
	}
	*clock = clock.Add(2 * time.Minute)
	newer, err := r.Claim(ctx, u.OwnerID, u.ID, uuid.NewString(), *clock, clock.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	reopened := NewMySQLUploads(r.db)
	if _, ok, err := reopened.BeginOriginalSeal(ctx, newer, "photo", *clock); err != nil || ok {
		t.Fatal("replacement lease issued COPY", ok, err)
	}
	if _, _, err := r.BeginOriginalSeal(ctx, claimed, "photo", *clock); !errors.Is(err, ErrLease) {
		t.Fatal("old lease accepted", err)
	}
	setDerivedAssetTrash(t, r.db, u, true, *clock)
	if err := reopened.RecordOriginalSeal(ctx, claimed, "photo", "late-version"); err != nil {
		t.Fatal("late receipt lost", err)
	}
	if err := reopened.RecordOriginalSeal(ctx, newer, "photo", "different-version"); !errors.Is(err, ErrConflict) {
		t.Fatal("immutable receipt overwritten", err)
	}
	if v, ok, err := reopened.OriginalSeal(ctx, u, "photo"); err != nil || !ok || v != "late-version" {
		t.Fatal(v, ok, err)
	}
}

func TestMySQLOriginalSealLostReplyRecoversWithoutStaging(t *testing.T) {
	r, s, o, u, clock := originalSealFixture(t)
	ctx := context.Background()
	storage := &originalSealFaultObjects{objectTestStore: o, fault: "lost-reply"}
	s.objects = storage
	if _, err := s.Verify(ctx, u.OwnerID, u.ID); !errors.Is(err, ErrCOSOperation) {
		t.Fatal(err)
	}
	before, reserved := photoJournalBalance(t, r.db, u.OwnerID)
	if before != 0 || reserved != u.OriginalBytes+u.DerivedBudget {
		t.Fatal("unknown COPY charged as complete")
	}
	delete(o.staging, stagingKey(u, "photo"))
	*clock = clock.Add(2 * time.Minute)
	s.repository = NewMySQLUploads(r.db)
	verified, err := s.Verify(ctx, u.OwnerID, u.ID)
	if err != nil || verified.State != OriginalsVerified || storage.copies != 1 {
		t.Fatal("restart did not reconcile once", err, storage.copies)
	}
	if _, err := s.Verify(ctx, u.OwnerID, u.ID); err != nil {
		t.Fatal(err)
	}
	used, left := photoJournalBalance(t, r.db, u.OwnerID)
	if used != u.OriginalBytes || left != u.DerivedBudget {
		t.Fatal("double charge", used, left)
	}
}

func TestMySQLOriginalSealFirstGrantRequiresCurrentLifecycleAndDurableWrite(t *testing.T) {
	for _, fault := range []string{"trash", "expired", "wrong-owner", "wrong-resource", "write-failure"} {
		t.Run(fault, func(t *testing.T) {
			r, _, _, u, clock := originalSealFixture(t)
			ctx := context.Background()
			claimed, err := r.Claim(ctx, u.OwnerID, u.ID, uuid.NewString(), *clock, clock.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			resource := "photo"
			switch fault {
			case "trash":
				setDerivedAssetTrash(t, r.db, u, true, *clock)
			case "expired":
				*clock = clock.Add(2 * time.Minute)
			case "wrong-owner":
				claimed.OwnerID = uuid.NewString()
			case "wrong-resource":
				resource = "foreign"
			case "write-failure":
				if _, err := r.db.Exec(`ALTER TABLE album_original_seals ADD CONSTRAINT fail_original_seal CHECK(resource_id<>'photo')`); err != nil {
					t.Fatal(err)
				}
			}
			if _, fresh, err := r.BeginOriginalSeal(ctx, claimed, resource, *clock); err == nil || fresh {
				t.Fatal("invalid grant", fresh, err)
			}
			var count int
			if err := r.db.QueryRow(`SELECT COUNT(*) FROM album_original_seals WHERE upload_id=?`, u.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed grant left intent", count, err)
			}
		})
	}
}

func TestMySQLOriginalSealMigrationDoesNotInventQuiescence(t *testing.T) {
	r, s, _, u, clock := originalSealFixture(t)
	ctx := context.Background()
	if _, err := s.Verify(ctx, u.OwnerID, u.ID); err != nil {
		t.Fatal(err)
	}
	pendingManifest := u.Manifest
	pendingManifest.AssetID = "pending-before-migration"
	pending, err := s.Create(ctx, u.OwnerID, uuid.NewString(), pendingManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(ctx, u.OwnerID, pending.ID); err != nil {
		t.Fatal(err)
	}
	pending, err = r.Claim(ctx, u.OwnerID, pending.ID, uuid.NewString(), *clock, clock.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`DROP TABLE album_original_seals`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`DELETE FROM schema_migrations WHERE name='0038_album_original_seals.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, r.db); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, r.db); err != nil {
		t.Fatal("migration replay", err)
	}
	if version, fresh, err := r.BeginOriginalSeal(ctx, pending, "photo", *clock); err != nil || fresh || version != "" {
		t.Fatal("pre-migration worker received another COPY", fresh, version, err)
	}
	var legacy bool
	var version string
	if err := r.db.QueryRow(`SELECT legacy,object_version FROM album_original_seals WHERE upload_id=? AND resource_id='photo'`, u.ID).Scan(&legacy, &version); err != nil || !legacy || version != "immutable-v1" {
		t.Fatal("bad migration", legacy, version, err)
	}
	if _, err := s.Verify(ctx, u.OwnerID, u.ID); err != nil {
		t.Fatal("migration broke old backup", err)
	}
	if _, err := r.db.Exec(`UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, *clock); err != nil {
		t.Fatal(err)
	}
	setDerivedAssetTrash(t, r.db, u, true, *clock)
	*clock = clock.Add(30 * 24 * time.Hour)
	if _, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, *clock); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("legacy COPY treated as bounded", err)
	}
}
