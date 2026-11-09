package albums

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func directUploadIssued(t *testing.T, r *MySQLUploads, kind, id string) bool {
	t.Helper()
	table, column := "album_uploads", "upload_id"
	if kind == "pair" {
		table, column = "album_live_pairs", "pair_upload_id"
	}
	var issued bool
	if err := r.db.QueryRow(`SELECT direct_upload_issued FROM `+table+` WHERE `+column+`=?`, id).Scan(&issued); err != nil {
		t.Fatal(err)
	}
	return issued
}

func TestMySQLDirectUploadEvidenceCommittedOnlyForIssuedDirectGrants(t *testing.T) {
	f := newSharingFixture(t)
	u := lifecycleUpload(t, f, "direct-grant")
	r, ctx, now := f.repo, context.Background(), *f.now
	if directUploadIssued(t, r, "original", u.ID) {
		t.Fatal("new upload treated as legacy")
	}
	failure := errors.New("signing failed")
	if err := r.WithUploadGrant(ctx, u, "", "", now, now.Add(time.Minute), func(time.Time) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if directUploadIssued(t, r, "original", u.ID) {
		t.Fatal("failed signing committed exposure")
	}
	intent, _, err := r.BeginMultipart(ctx, u.OwnerID, u.ID, "photo", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ResolveMultipart(ctx, u.OwnerID, u.ID, "photo", intent.IntentID, "provider-upload", now); err != nil {
		t.Fatal(err)
	}
	if err = r.WithUploadGrant(ctx, u, "photo", "provider-upload", now, now.Add(time.Minute), func(time.Time) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if directUploadIssued(t, r, "original", u.ID) {
		t.Fatal("multipart grant treated as direct PUT")
	}
	if err = r.WithUploadGrant(ctx, u, "", "", now, now.Add(time.Minute), func(time.Time) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !directUploadIssued(t, r, "original", u.ID) {
		t.Fatal("direct URL exposure not committed")
	}
	// A later signing failure or passing the signature deadline cannot clear it.
	if err = r.WithUploadGrant(ctx, u, "", "", now.Add(2*time.Minute), now.Add(3*time.Minute), func(time.Time) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if !directUploadIssued(t, r, "original", u.ID) {
		t.Fatal("issued URL lost after expiry")
	}
}

func TestMySQLDirectUploadGrantCannotEscapeFailedEvidenceWrite(t *testing.T) {
	f := newSharingFixture(t)
	u := lifecycleUpload(t, f, "direct-commit-failure")
	r := f.repo
	if _, err := f.db.Exec(`ALTER TABLE album_uploads RENAME COLUMN direct_upload_issued TO hidden_direct_upload_issued`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := f.db.Exec(`ALTER TABLE album_uploads RENAME COLUMN hidden_direct_upload_issued TO direct_upload_issued`); err != nil {
			t.Error(err)
		}
	}()
	calls := 0
	err := r.WithUploadGrant(context.Background(), u, "", "", *f.now, f.now.Add(time.Minute), func(time.Time) error { calls++; return nil })
	if err == nil || calls != 1 {
		t.Fatal("URL could escape without evidence commit", err, calls)
	}
}

func TestMySQLLivePairDirectGrantEvidenceAndLegacyMigration(t *testing.T) {
	f := newPushFixture(t)
	r := NewMySQLUploads(f.r.db)
	u, descriptor := livePairJournalFixture(t, f.r.db)
	now, ctx := time.Now().UTC().Truncate(time.Microsecond), context.Background()
	service := livePairTestService(t, r, &now, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected network"); return nil, nil })
	pair, err := service.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if directUploadIssued(t, r, "pair", pair.ID) || directUploadIssued(t, r, "original", u.ID) {
		t.Fatal("new controlled records marked legacy")
	}
	failure := errors.New("signing failed")
	if err = r.WithLivePairUploadGrant(ctx, pair, now, now.Add(time.Minute), func(time.Time) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if directUploadIssued(t, r, "pair", pair.ID) {
		t.Fatal("failed pair grant committed exposure")
	}
	grants, err := service.LivePairGrants(ctx, u.OwnerID, pair.ID)
	if err != nil || len(grants) != 2 || !directUploadIssued(t, r, "pair", pair.ID) {
		t.Fatal("pair grant exposure missing", err)
	}
	if _, err = f.r.db.Exec(`ALTER TABLE album_uploads DROP COLUMN direct_upload_issued; ALTER TABLE album_live_pairs DROP COLUMN direct_upload_issued`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/0049_album_direct_upload_evidence.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.r.db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if !directUploadIssued(t, r, "original", u.ID) || !directUploadIssued(t, r, "pair", pair.ID) {
		t.Fatal("old rows guessed to have no direct grants")
	}
	if err = r.CheckLibrarySchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err = r.CheckLivePairSchema(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLDirectUploadEvidenceStopsOldPurgePlansAndNewAdmission(t *testing.T) {
	db, u, now := purgeFixture(t)
	r, ctx := NewMySQLUploads(db), context.Background()
	p, err := r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now)
	if err != nil {
		t.Fatal(err)
	}
	p, err = r.claimObjectPurge(ctx, p.OwnerID, p.AssetID, uuid.NewString(), now)
	if err != nil {
		t.Fatal(err)
	}
	p, err = r.storeObjectPurgePlan(ctx, p, purgePlan{Versions: []DerivedObjectVersion{{"planned-version", false, 100}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Represents an old plan reloaded after conservative migration of its upload.
	if _, err = db.Exec(`UPDATE album_uploads SET direct_upload_issued=TRUE WHERE upload_id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.authorizeObjectPurgeAction(ctx, p, func() time.Time { return now }); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("old plan allowed deletion", err)
	}
	if _, err = r.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, now.Add(time.Hour)); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("old plan replay bypassed exposure", err)
	}
	if _, err = r.claimObjectPurge(ctx, p.OwnerID, p.AssetID, uuid.NewString(), now.Add(time.Hour)); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("expired lease bypassed exposure", err)
	}
	if _, err = r.ackObjectPurgeAction(ctx, p, now); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("action acknowledged without termination proof", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = closedPurgeTargets(ctx, tx, u.OwnerID, u.Manifest.AssetID, now.Add(365*24*time.Hour)); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("elapsed time treated as PUT termination", err)
	}
}

func TestMySQLLivePairDirectExposurePreventsAssetPurge(t *testing.T) {
	f := newPushFixture(t)
	r, ctx := NewMySQLUploads(f.r.db), context.Background()
	u, descriptor := livePairJournalFixture(t, f.r.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := livePairTestService(t, r, &now, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected network"); return nil, nil })
	pair, err := service.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.LivePairGrants(ctx, u.OwnerID, pair.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = requireNoDirectUploads(ctx, tx, u.OwnerID, u.Manifest.AssetID); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("pair direct upload bypassed gate", err)
	}
	if err = requireNoDirectUploads(ctx, tx, u.OwnerID, "different-asset"); err != nil {
		t.Fatal("another asset blocked", err)
	}
}
