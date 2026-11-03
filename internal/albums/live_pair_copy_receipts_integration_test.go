package albums

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMySQLLivePairCopyReceiptSurvivesPreflightAndVerificationFailures(t *testing.T) {
	for _, mode := range []string{"preflight", "download", "cancel-after-copy"} {
		t.Run(mode, func(t *testing.T) {
			db := photoJournalDB(t)
			u, descriptor := livePairJournalFixture(t, db)
			r, now := NewMySQLUploads(db), time.Now().UTC().Truncate(time.Microsecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failed, recovering, copies := false, false, 0
			s := livePairTestService(t, r, &now, func(req *http.Request) (*http.Response, error) {
				part, data := "photo", "paired-jpeg"
				if strings.HasSuffix(req.URL.Path, "/motion") {
					part, data = "motion", "paired-quicktime"
				}
				version := "sealed-" + part
				switch req.Method {
				case http.MethodHead:
					if !strings.Contains(req.URL.Path, "/staging/") || recovering && mode != "preflight" && part == "photo" {
						t.Fatal("saved receipt ignored: unnecessary HEAD")
					}
					if mode == "preflight" && !failed {
						failed = true
						return nil, io.ErrUnexpectedEOF
					}
					response := cosResponse(req, "", "stage-"+part)
					response.ContentLength = int64(len(data))
					return response, nil
				case http.MethodPut:
					copies++
					if !strings.HasSuffix(req.Header.Get("x-cos-copy-source"), "?versionId=stage-"+part) {
						t.Error("source version not pinned")
					}
					if mode == "cancel-after-copy" && !failed {
						failed = true
						cancel()
					}
					return cosResponse(req, `<CopyObjectResult><ETag>ok</ETag><VersionId>`+version+`</VersionId></CopyObjectResult>`, ""), nil
				case http.MethodGet:
					if err := req.Context().Err(); err != nil {
						return nil, err
					}
					if mode == "download" && !failed {
						failed = true
						return nil, io.ErrUnexpectedEOF
					}
					if req.URL.Query().Get("versionId") != version {
						t.Error("read not pinned to COPY receipt")
					}
					return cosResponse(req, data, version), nil
				default:
					t.Fatal("unexpected provider request", req.Method)
					return nil, nil
				}
			})
			before, _ := photoJournalBalance(t, db, u.OwnerID)
			p, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.SubmitLivePair(ctx, u.OwnerID, p.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = s.VerifyLivePair(ctx, u.OwnerID, p.ID); err == nil {
				t.Fatal("failure published pair")
			}
			p, err = r.LivePairByID(context.Background(), u.OwnerID, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Objects) != 0 || p.State == "verified" || p.MovieConsumed {
				t.Fatal("failed verification settled pair", p.State)
			}
			if mode == "preflight" {
				if p.PhotoConsumed || p.PhotoCopyVersion != "" || copies != 0 {
					t.Fatal("preflight consumed COPY intent")
				}
			} else if !p.PhotoConsumed || p.PhotoCopyVersion != "sealed-photo" || copies != 1 {
				t.Fatal("positive receipt lost", p.PhotoCopyVersion, copies)
			}
			used, reserved := photoJournalBalance(t, db, u.OwnerID)
			size := descriptor.Photo.SizeBytes + descriptor.Movie.SizeBytes
			if used != before || reserved != size {
				t.Fatal("unverified receipt settled quota")
			}
			recovering = true
			now = now.Add(2 * time.Minute)
			ready, err := s.VerifyLivePair(context.Background(), u.OwnerID, p.ID)
			if err != nil || ready.State != "verified" || copies != 2 {
				t.Fatal("retry failed or repeated COPY", err, copies)
			}
			used, reserved = photoJournalBalance(t, db, u.OwnerID)
			if used != before+size || reserved != 0 {
				t.Fatal("verified quota not settled once")
			}
		})
	}
}

func TestMySQLLivePairLateCopyReceiptIsBoundAndDoesNotPublish(t *testing.T) {
	f := newPushFixture(t)
	db := f.r.db
	u, descriptor := livePairJournalFixture(t, db)
	r, ctx, now := NewMySQLUploads(db), context.Background(), time.Now().UTC().Truncate(time.Microsecond)
	s := livePairTestService(t, r, &now, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected provider call"); return nil, nil })
	p, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitLivePair(ctx, u.OwnerID, p.ID); err != nil {
		t.Fatal(err)
	}
	p, err = r.ClaimLivePair(ctx, u.OwnerID, p.ID, uuid.NewString(), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = r.BeginLivePairSeal(ctx, p, "photo", now)
	if err != nil {
		t.Fatal(err)
	}
	next, err := r.ClaimLivePair(ctx, u.OwnerID, p.ID, uuid.NewString(), p.LeaseUntil.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	deleteProtocolFixtureOwner(t, f, u.OwnerID)
	usedBefore, reservedBefore := photoJournalBalance(t, db, u.OwnerID)
	for _, change := range []func(*LivePairUpload){
		func(v *LivePairUpload) { v.ManifestDigest = strings.Repeat("f", 64) },
		func(v *LivePairUpload) { v.Descriptor.Pair.Identifier = uuid.NewString() },
		func(v *LivePairUpload) { v.SourcePhoto.VersionID = "other-version" },
		func(v *LivePairUpload) { v.SourceVideo.VersionID = "other-version" },
		func(v *LivePairUpload) { v.VersionID = uuid.NewString() },
		func(v *LivePairUpload) { v.AssetID = "other-asset" },
		func(v *LivePairUpload) { v.OwnerID = uuid.NewString() },
	} {
		bad := p
		change(&bad)
		if _, err = r.RecordLivePairSeal(ctx, bad, "photo", "late-version"); err == nil {
			t.Fatal("unbound receipt accepted")
		}
	}
	if _, err = r.RecordLivePairSeal(ctx, p, "motion", "unissued"); !errors.Is(err, ErrConflict) {
		t.Fatal("unissued receipt accepted", err)
	}
	for range 2 {
		if _, err = r.RecordLivePairSeal(ctx, p, "photo", "late-version"); err != nil {
			t.Fatal("late positive receipt lost", err)
		}
	}
	if _, err = r.RecordLivePairSeal(ctx, p, "photo", "conflict-version"); !errors.Is(err, ErrConflict) {
		t.Fatal("positive receipt overwritten", err)
	}
	current, err := r.LivePairByID(ctx, p.OwnerID, p.ID)
	if err != nil || current.PhotoCopyVersion != "late-version" || current.LeaseToken != next.LeaseToken || len(current.Objects) != 0 || current.State != "verifying" {
		t.Fatal("receipt mutated verification or lease", err)
	}
	key, _ := p.key("photo", false)
	receipt := SealedObject{"photo", key, "late-version", p.Descriptor.Photo.SizeBytes, p.Descriptor.Photo.SHA256}
	if _, err = r.CompleteLivePairPart(ctx, p, receipt, next.LeaseUntil.Add(-time.Second)); !errors.Is(err, ErrLease) {
		t.Fatal("stale worker published", err)
	}
	if _, err = s.LivePairReadGrant(ctx, p.OwnerID, p.VersionID); err == nil {
		t.Fatal("deleted account received read grant")
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != usedBefore || reserved != reservedBefore {
		t.Fatal("raw receipt charged quota")
	}
}

func TestMySQLLivePairCopyReceiptMigrationAndAtomicCompletion(t *testing.T) {
	// Isolated schema: this test temporarily removes columns to exercise old-row migration.
	f := newPushFixture(t)
	db := f.r.db
	u, descriptor := livePairJournalFixture(t, db)
	r, ctx, now := NewMySQLUploads(db), context.Background(), time.Now().UTC().Truncate(time.Microsecond)
	s := livePairTestService(t, r, &now, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected provider call"); return nil, nil })
	p, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitLivePair(ctx, u.OwnerID, p.ID); err != nil {
		t.Fatal(err)
	}
	p, err = r.ClaimLivePair(ctx, u.OwnerID, p.ID, uuid.NewString(), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"photo", "motion"} {
		p, _, err = r.BeginLivePairSeal(ctx, p, part, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	key, _ := p.key("photo", false)
	receipt := SealedObject{"photo", key, "verified-photo", descriptor.Photo.SizeBytes, descriptor.Photo.SHA256}
	if _, err = db.Exec(`ALTER TABLE album_live_pairs RENAME COLUMN updated_at TO hidden_updated_at`); err != nil {
		t.Fatal(err)
	}
	_, failed := r.CompleteLivePairPart(ctx, p, receipt, now)
	if _, err = db.Exec(`ALTER TABLE album_live_pairs RENAME COLUMN hidden_updated_at TO updated_at`); err != nil {
		t.Fatal(err)
	}
	current, err := r.LivePairByID(ctx, p.OwnerID, p.ID)
	if failed == nil || err != nil || current.PhotoCopyVersion != "" || len(current.Objects) != 0 {
		t.Fatal("failed completion partially persisted receipt", failed, err)
	}
	p, err = r.CompleteLivePairPart(ctx, p, receipt, now)
	if err != nil {
		t.Fatal(err)
	}
	bad := receipt
	bad.VersionID = "substituted"
	if _, err = r.CompleteLivePairPart(ctx, p, bad, now); !errors.Is(err, ErrConflict) {
		t.Fatal("verified version replaced", err)
	}
	if _, err = db.Exec(`ALTER TABLE album_live_pairs DROP CHECK album_live_photo_copy_version_ck,DROP CHECK album_live_movie_copy_version_ck,DROP COLUMN photo_copy_version,DROP COLUMN movie_copy_version`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/0046_album_live_pair_copy_receipts.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	current, err = r.LivePairByID(ctx, p.OwnerID, p.ID)
	if err != nil || current.PhotoCopyVersion != receipt.VersionID || current.MovieCopyVersion != "" || !current.MovieConsumed || len(current.Objects) != 1 {
		t.Fatal("migration fabricated or lost receipt", err)
	}
}
