package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func purgeFixture(t *testing.T) (*sql.DB, Upload, time.Time) {
	t.Helper()
	db, _ := gatewayTestDatabase(t)
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	u, _ := photoJournalUpload(t, db)
	t.Cleanup(func() { db.Exec(`DELETE FROM album_object_purges WHERE owner_id=?`, u.OwnerID) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.Exec(`UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMySQLUploads(db).FinalizeDerivedBudget(context.Background(), DerivedVersionRef{u.OwnerID, u.ID}, now); err != nil {
		t.Fatal(err)
	}
	setDerivedAssetTrash(t, db, u, true, now)
	return db, u, now.Add(30 * 24 * time.Hour)
}

func TestMySQLObjectPurgeAdmissionRequiresExpiredClosedJournals(t *testing.T) {
	db, u, now := purgeFixture(t)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	if _, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now.Add(-time.Microsecond)); !errors.Is(err, ErrConflict) {
		t.Fatal("early trash purged", err)
	}
	if _, err := r.prepareExpiredTrashPurge(ctx, uuid.NewString(), u.Manifest.AssetID, now); !errors.Is(err, ErrOwner) {
		t.Fatal("foreign owner admitted", err)
	}
	for _, fault := range []struct{ apply, undo string }{
		{`UPDATE album_uploads SET derived_budget_closed=FALSE WHERE owner_id=?`, `UPDATE album_uploads SET derived_budget_closed=TRUE WHERE owner_id=?`},
		{`UPDATE album_photo_jobs SET state='queued' WHERE owner_id=?`, `UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`},
		{`UPDATE album_derived_finalizations SET held_bytes=1,released_bytes=released_bytes-1 WHERE owner_id=?`, `UPDATE album_derived_finalizations SET held_bytes=0,released_bytes=released_bytes+1 WHERE owner_id=?`},
	} {
		if _, err := db.Exec(fault.apply, u.OwnerID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now); !errors.Is(err, ErrDerivedPending) {
			t.Fatal("unresolved work admitted", err)
		}
		if _, err := db.Exec(fault.undo, u.OwnerID); err != nil {
			t.Fatal(err)
		}
	}
	p, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now)
	if err != nil || len(p.Targets) != 2 {
		t.Fatal("closed trash not admitted", p, err)
	}
	db.SetMaxOpenConns(1)
	again, err := NewMySQLUploads(db).prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(p, again) {
		t.Fatal("restart changed immutable targets", err)
	}
	var winners atomic.Int32
	var group sync.WaitGroup
	for range 6 {
		group.Go(func() {
			_, err := r.claimObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, uuid.NewString(), now)
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrLease) {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if winners.Load() != 1 {
		t.Fatal("multiple leased writers", winners.Load())
	}
}

type purgeStorageFixture struct {
	t               *testing.T
	r               *MySQLUploads
	asset           string
	versions        map[string][]DerivedObjectVersion
	uploads         map[string][]string
	deletes, aborts []string
	loseOnce        bool
	loseID          string
	beforeList      func()
}

func (s *purgeStorageFixture) listPurgeVersions(_ context.Context, _ string, t purgeTarget) ([]DerivedObjectVersion, error) {
	if s.beforeList != nil {
		f := s.beforeList
		s.beforeList = nil
		f()
	}
	return append([]DerivedObjectVersion(nil), s.versions[t.Key]...), nil
}
func (s *purgeStorageFixture) listPurgeUploads(_ context.Context, _ string, t purgeTarget) ([]string, error) {
	return append([]string(nil), s.uploads[t.Key]...), nil
}
func (s *purgeStorageFixture) deletePurgeVersion(ctx context.Context, owner string, t purgeTarget, v DerivedObjectVersion) error {
	p, err := s.r.objectPurge(ctx, owner, s.asset)
	if err != nil || p.Plan == nil || p.Targets[p.Index] != t {
		s.t.Fatal("DELETE before durable plan", err)
	}
	found := false
	for _, item := range p.Plan.Versions {
		if item == v {
			found = true
		}
	}
	if !found {
		s.t.Fatal("DELETE absent from plan")
	}
	s.deletes = append(s.deletes, v.VersionID)
	var remaining []DerivedObjectVersion
	for _, item := range s.versions[t.Key] {
		if item.VersionID != v.VersionID {
			remaining = append(remaining, item)
		}
	}
	s.versions[t.Key] = remaining
	if s.loseOnce || v.VersionID == s.loseID {
		s.loseOnce = false
		s.loseID = ""
		return ErrCOSOperation
	}
	return nil
}
func (s *purgeStorageFixture) abortPurgeUpload(ctx context.Context, owner string, t purgeTarget, id string) error {
	p, err := s.r.objectPurge(ctx, owner, s.asset)
	if err != nil || p.Plan == nil || len(p.Plan.Uploads) != 1 || p.Plan.Uploads[0] != id {
		s.t.Fatal("abort without durable plan", err)
	}
	s.aborts = append(s.aborts, id)
	delete(s.uploads, t.Key)
	return nil
}
func TestMySQLObjectPurgeResumesExactPlanThenRelistsWithoutRefund(t *testing.T) {
	db, u, now := purgeFixture(t)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	p, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now)
	if err != nil {
		t.Fatal(err)
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	key := p.Targets[0].Key
	s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID, versions: map[string][]DerivedObjectVersion{key: {{"first", false, 3}, {"marker", true, 0}}}, uploads: map[string][]string{key: {"multipart-one"}}}
	clock := func() time.Time { return now }
	if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, clock); err != nil {
		t.Fatal(err)
	}
	if len(s.aborts) != 1 || len(s.deletes) != 0 {
		t.Fatal("did not abort before versions")
	}
	s.loseOnce = true
	if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, clock); !errors.Is(err, ErrCOSOperation) {
		t.Fatal("lost response accepted", err)
	}
	before, _ := r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
	if before.Plan == nil || before.Index != 0 {
		t.Fatal("lost progress on failure")
	}
	now = now.Add(11 * time.Minute)
	r = NewMySQLUploads(db)
	s.r = r
	if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, clock); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.deletes, []string{"first", "first", "marker"}) {
		t.Fatal("replay changed plan", s.deletes)
	}
	// Another historical version is discovered by the next inventory, not lost
	// because the prior batch was acknowledged.
	s.versions[key] = []DerivedObjectVersion{{"later-observed", false, 2}}
	for range 4 {
		if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, clock); err != nil {
			t.Fatal(err)
		}
		p, _ = r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
		if p.State == "objects_removed" {
			break
		}
	}
	if p.State != "objects_removed" || len(s.deletes) != 4 {
		t.Fatal("did not exhaust all keys", p.State, s.deletes)
	}
	u2, r2 := photoJournalBalance(t, db, u.OwnerID)
	if u2 != used || r2 != reserved {
		t.Fatal("object observation refunded quota")
	}
	if _, err := r.AssetVersion(ctx, u.OwnerID, u.ID); err != nil {
		t.Fatal("object worker erased metadata", err)
	}
	if _, err := r.authorizeObjectPurgeAction(ctx, before, clock); !errors.Is(err, ErrLease) {
		t.Fatal("old lease resurrected", err)
	}
}

func TestMySQLObjectPurgeAcknowledgedActionsSurviveLeaseReplacement(t *testing.T) {
	db, u, now := purgeFixture(t)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	p, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now)
	if err != nil {
		t.Fatal(err)
	}
	s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID, loseID: "second", versions: map[string][]DerivedObjectVersion{p.Targets[0].Key: {{"first", false, 1}, {"second", false, 1}, {"third", false, 1}}}}
	clock := func() time.Time { return now }
	if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, clock); !errors.Is(err, ErrCOSOperation) {
		t.Fatal(err)
	}
	p, err = r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
	if err != nil || p.ActionIndex != 1 || p.Plan == nil {
		t.Fatal("acknowledged progress lost", p.ActionIndex, err)
	}
	if err := r.finishObjectPurgeStep(ctx, p, false, now); !errors.Is(err, ErrConflict) {
		t.Fatal("unfinished batch accepted", err)
	}
	now = now.Add(11 * time.Minute)
	if err := NewMySQLUploads(db).runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, clock); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.deletes, []string{"first", "second", "second", "third"}) {
		t.Fatal("replayed already acknowledged prefix", s.deletes)
	}
	if _, err := r.ackObjectPurgeAction(ctx, p, now); !errors.Is(err, ErrLease) {
		t.Fatal("stale worker advanced cursor", err)
	}
}

func TestMySQLObjectPurgeEmptyPlanCrashMustRelist(t *testing.T) {
	db, u, now := purgeFixture(t)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	if _, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now); err != nil {
		t.Fatal(err)
	}
	p, err := r.claimObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, uuid.NewString(), now)
	if err != nil {
		t.Fatal(err)
	}
	p, err = r.storeObjectPurgePlan(ctx, p, purgePlan{}, now)
	if err != nil {
		t.Fatal(err)
	}
	s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID, versions: map[string][]DerivedObjectVersion{p.Targets[0].Key: {{"after-crash", false, 4}}}}
	now = now.Add(11 * time.Minute)
	if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	p, err = r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
	if err != nil || p.Index != 0 || p.Plan != nil || len(s.deletes) != 0 {
		t.Fatal("stale absence advanced key", p, err)
	}
	if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.deletes, []string{"after-crash"}) {
		t.Fatal("new object missed", s.deletes)
	}
}

func TestMySQLObjectPurgeLifecycleAndPlanFences(t *testing.T) {
	db, u, now := purgeFixture(t)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	p, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now)
	if err != nil {
		t.Fatal(err)
	}
	s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID, versions: map[string][]DerivedObjectVersion{p.Targets[0].Key: {{"keep", false, 4}}}}
	s.beforeList = func() { setDerivedAssetTrash(t, db, u, false, now) }
	if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); !errors.Is(err, ErrConflict) {
		t.Fatal("changed lifecycle accepted", err)
	}
	if len(s.deletes) != 0 {
		t.Fatal("deleted after lifecycle changed")
	}
}

func TestMySQLObjectPurgePlanWriteFailureAndExpiredLeaseNeverDelete(t *testing.T) {
	for _, fault := range []string{"journal-write", "lease-expired"} {
		t.Run(fault, func(t *testing.T) {
			db, u, now := purgeFixture(t)
			r := NewMySQLUploads(db)
			ctx := context.Background()
			p, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now)
			if err != nil {
				t.Fatal(err)
			}
			s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID, versions: map[string][]DerivedObjectVersion{p.Targets[0].Key: {{"keep", false, 4}}}}
			if fault == "journal-write" {
				if _, err := db.Exec(`ALTER TABLE album_object_purges MODIFY plan_json VARCHAR(1) NULL`); err != nil {
					t.Fatal(err)
				}
			} else {
				s.beforeList = func() { now = now.Add(11 * time.Minute) }
			}
			if err := r.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, s, func() time.Time { return now }); err == nil {
				t.Fatal("failure ignored")
			}
			p, err = r.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
			if err != nil || p.Plan != nil || len(s.deletes) != 0 {
				t.Fatal("side effect without durable live lease", err)
			}
		})
	}
}

func TestMySQLObjectPurgeIncludesLivePhotoComponentsAndPair(t *testing.T) {
	db, _ := gatewayTestDatabase(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	u, descriptor := livePairJournalFixture(t, db)
	t.Cleanup(func() { db.Exec(`DELETE FROM album_object_purges WHERE owner_id=?`, u.OwnerID) })
	r := NewMySQLUploads(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	s := livePairTestService(t, r, &now, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected provider request"); return nil, nil })
	pair, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitLivePair(ctx, u.OwnerID, pair.ID); err != nil {
		t.Fatal(err)
	}
	pair, err = r.ClaimLivePair(ctx, u.OwnerID, pair.ID, uuid.NewString(), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"photo", "motion"} {
		pair, _, err = r.BeginLivePairSeal(ctx, pair, id, now)
		if err != nil {
			t.Fatal(err)
		}
		resource, _ := pair.resource(id)
		key, _ := pair.key(id, false)
		pair, err = r.CompleteLivePairPart(ctx, pair, SealedObject{ResourceID: id, Key: key, VersionID: "pair-" + id, SizeBytes: resource.SizeBytes, SHA256: resource.SHA256}, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	pair, err = r.CompleteLivePair(ctx, pair, now)
	if err != nil {
		t.Fatal(err)
	}
	setDerivedAssetTrash(t, db, u, true, now)
	now = now.Add(30 * 24 * time.Hour)
	if _, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("unfinished derivative retirement admitted", err)
	}
	// Seed completed provider-retirement observations for this admission-only
	// fixture; provider execution and lost replies are exercised separately.
	if _, err := db.Exec(`UPDATE album_derived_cleanups SET state='completed' WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE album_live_pairs SET state='attention' WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("uncertain Live Photo admitted", err)
	}
	if _, err := db.Exec(`UPDATE album_live_pairs SET state='verified' WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	p, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now)
	if err != nil || len(p.Targets) != 11 {
		t.Fatal("incomplete Live Photo targets", len(p.Targets), err)
	}
	keys := map[string]bool{}
	for _, target := range p.Targets {
		keys[target.Key] = true
	}
	for _, id := range []string{"photo", "motion"} {
		for _, staging := range []bool{true, false} {
			key, _ := pair.key(id, staging)
			if !keys[key] {
				t.Fatal("missing paired object", key)
			}
		}
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	storage := drainPurgeFixture(t, r, u, now)
	refund, err := r.settleObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, storage, func() time.Time { return now })
	if err != nil || refund != used || reserved != 0 {
		t.Fatal("Live Photo settlement omitted components", refund, used, reserved, err)
	}
	after, held := photoJournalBalance(t, db, u.OwnerID)
	if after != 0 || held != 0 {
		t.Fatal("Live Photo quota retained", after, held)
	}
}
