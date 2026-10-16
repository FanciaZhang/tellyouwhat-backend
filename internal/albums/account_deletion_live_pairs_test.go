package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func deletionLivePairFixture(t *testing.T, parts []string, deleted bool) (*pushFixture, *MySQLUploads, livePairDeletionCheck) {
	t.Helper()
	f := newPushFixture(t)
	r := NewMySQLUploads(f.r.db)
	u, descriptor := livePairJournalFixture(t, f.r.db)
	now, ctx := time.Now().UTC().Truncate(time.Microsecond), context.Background()
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
	for _, part := range parts {
		p, _, err = r.BeginLivePairSeal(ctx, p, part, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = r.DeferLivePair(ctx, p, now); err != nil {
		t.Fatal(err)
	}
	c := livePairDeletionCheck{account: p.OwnerID, id: p.ID, pair: p}
	if deleted {
		deleteProtocolFixtureOwner(t, f, p.OwnerID)
		if err = f.r.db.QueryRow(`SELECT request_id FROM album_account_deletions WHERE account_id=?`, p.OwnerID).Scan(&c.request); err != nil {
			t.Fatal(err)
		}
	}
	return f, r, c
}

func assertDeletionLivePair(t *testing.T, r *MySQLUploads, c livePairDeletionCheck, photo, movie string) {
	t.Helper()
	p, err := r.LivePairByID(context.Background(), c.account, c.id)
	if err != nil || p.PhotoCopyVersion != photo || p.MovieCopyVersion != movie || len(p.Objects) != 0 || p.State != "queued" || p.LeaseToken != "" {
		t.Fatal("raw observations changed verification state", p.State, err)
	}
}

func TestMySQLLivePairDeletionPersistsPartialAndResumesWithoutRepeatingReads(t *testing.T) {
	f, r, c := deletionLivePairFixture(t, []string{"photo", "motion"}, true)
	ctx := context.Background()
	before, reservedBefore := photoJournalBalance(t, f.r.db, c.account)
	calls := []string{}
	observer := func(ctx context.Context, p LivePairUpload, part string) (string, error) {
		calls = append(calls, part)
		if !sameLivePairCopy(p, c.pair) || p.State != "queued" {
			t.Fatal("wrong pair observed")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("unbounded query")
		}
		if part == "motion" {
			return "", ErrCOSOperation
		}
		return "photo-version", nil
	}
	b, err := r.runLivePairDeletionBatch(ctx, observer, 10, func() time.Time { return f.now })
	if err != nil || b.Deferred != 1 || len(calls) != 2 {
		t.Fatal(b, err, calls)
	}
	assertDeletionLivePair(t, r, c, "photo-version", "")
	b, err = r.runLivePairDeletionBatch(ctx, observer, 10, func() time.Time { return f.now.Add(59 * time.Second) })
	if err != nil || b != (livePairDeletionBatch{}) || len(calls) != 2 {
		t.Fatal("backoff ignored", b, err)
	}
	reopened := NewMySQLUploads(f.r.db)
	b, err = reopened.runLivePairDeletionBatch(ctx, func(_ context.Context, _ LivePairUpload, part string) (string, error) {
		if part != "motion" {
			t.Fatal("known receipt queried again")
		}
		return "motion-version", nil
	}, 10, func() time.Time { return f.now.Add(time.Minute) })
	if err != nil || b.Acknowledged != 1 {
		t.Fatal("restart failed to resume missing component", b, err)
	}
	assertDeletionLivePair(t, r, c, "photo-version", "motion-version")
	b, err = reopened.runLivePairDeletionBatch(ctx, observer, 10, func() time.Time { return f.now.Add(time.Hour) })
	if err != nil || b != (livePairDeletionBatch{}) || len(calls) != 2 {
		t.Fatal("complete observation repeated", b, err)
	}
	used, reserved := photoJournalBalance(t, f.r.db, c.account)
	if used != before || reserved != reservedBefore {
		t.Fatal("observation settled quota")
	}
}

func TestMySQLLivePairDeletionSkipsActiveUnissuedAndKnown(t *testing.T) {
	for _, mode := range []string{"active", "unissued", "known"} {
		t.Run(mode, func(t *testing.T) {
			parts := []string{"photo"}
			if mode == "unissued" {
				parts = nil
			}
			f, r, c := deletionLivePairFixture(t, parts, mode != "active")
			if mode == "known" {
				if _, err := r.RecordLivePairSeal(context.Background(), c.pair, "photo", "known"); err != nil {
					t.Fatal(err)
				}
			}
			b, err := r.runLivePairDeletionBatch(context.Background(), func(context.Context, LivePairUpload, string) (string, error) {
				t.Fatal("ineligible pair queried")
				return "", nil
			}, 10, func() time.Time { return f.now.Add(time.Hour) })
			if err != nil || b != (livePairDeletionBatch{}) {
				t.Fatal(b, err)
			}
		})
	}
}

func TestMySQLLivePairDeletionUnknownResultsCannotAcknowledge(t *testing.T) {
	for _, mode := range []string{"empty", "null", "oversize", "error-with-version", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, r, c := deletionLivePairFixture(t, []string{"motion"}, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b, err := r.runLivePairDeletionBatch(ctx, func(_ context.Context, _ LivePairUpload, part string) (string, error) {
				if part != "motion" {
					t.Fatal("unissued photo queried")
				}
				switch mode {
				case "empty":
					return "", nil
				case "null":
					return "null", nil
				case "oversize":
					return strings.Repeat("v", 1025), nil
				case "error-with-version":
					return "uncertain", ErrCOSOperation
				default:
					cancel()
					return "cancelled", nil
				}
			}, 1, func() time.Time { return f.now })
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if err != nil || b.Deferred != 1 {
				t.Fatal("unknown result acknowledged", b, err)
			}
			assertDeletionLivePair(t, r, c, "", "")
		})
	}
}

func TestMySQLLivePairDeletionLeaseConflictRollsBackBothComponents(t *testing.T) {
	f, r, c := deletionLivePairFixture(t, []string{"photo", "motion"}, true)
	ctx := context.Background()
	first, err := r.claimLivePairDeletion(ctx, c, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.claimLivePairDeletion(ctx, c, f.now.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("live lease stolen", err)
	}
	observation := livePairDeletionObservation{"found-photo", "found-movie"}
	if _, err = r.finishLivePairDeletion(ctx, first, observation, f.now.Add(livePairDeletionLease)); !errors.Is(err, ErrConflict) {
		t.Fatal("expired receipt saved", err)
	}
	assertDeletionLivePair(t, r, c, "", "")
	second, err := r.claimLivePairDeletion(ctx, c, f.now.Add(livePairDeletionLease))
	if err != nil || first.lease == second.lease {
		t.Fatal("lease not recovered", err)
	}
	if _, err = r.finishLivePairDeletion(ctx, first, observation, f.now.Add(livePairDeletionLease)); !errors.Is(err, ErrConflict) {
		t.Fatal("stale receipt saved", err)
	}
	assertDeletionLivePair(t, r, c, "", "")
	if _, err = r.RecordLivePairSeal(ctx, c.pair, "motion", "late-movie"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.finishLivePairDeletion(ctx, second, observation, f.now.Add(livePairDeletionLease+time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting receipt overwritten", err)
	}
	assertDeletionLivePair(t, r, c, "", "late-movie")
	// A failed motion HEAD cannot erase the independent late positive receipt.
	observation.movie = ""
	ack, err := r.finishLivePairDeletion(ctx, second, observation, f.now.Add(livePairDeletionLease+time.Second))
	if err != nil || !ack {
		t.Fatal("late receipt not retained", ack, err)
	}
	assertDeletionLivePair(t, r, c, "found-photo", "late-movie")
}

func TestMySQLLivePairDeletionRejectsChangedBinding(t *testing.T) {
	f, r, c := deletionLivePairFixture(t, []string{"photo"}, true)
	ctx := context.Background()
	claimed, err := r.claimLivePairDeletion(ctx, c, f.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*livePairDeletionCheck){
		func(v *livePairDeletionCheck) { v.account = uuid.NewString() },
		func(v *livePairDeletionCheck) { v.request = uuid.NewString() },
		func(v *livePairDeletionCheck) { v.id = uuid.NewString() },
		func(v *livePairDeletionCheck) { v.pair.ManifestDigest = strings.Repeat("d", 64) },
		func(v *livePairDeletionCheck) { v.pair.Descriptor.Pair.Identifier = uuid.NewString() },
		func(v *livePairDeletionCheck) { v.pair.SourcePhoto.VersionID = "substituted" },
		func(v *livePairDeletionCheck) { v.pair.SourceVideo.VersionID = "substituted" },
		func(v *livePairDeletionCheck) { v.pair.MovieConsumed = true },
	} {
		bad := claimed
		change(&bad)
		if _, err = r.finishLivePairDeletion(ctx, bad, livePairDeletionObservation{photo: "wrong"}, f.now); err == nil {
			t.Fatal("changed binding accepted")
		}
		assertDeletionLivePair(t, r, c, "", "")
	}
	if _, err = f.r.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=NULL WHERE owner_id=?`, c.account); err != nil {
		t.Fatal(err)
	}
	if _, err = r.finishLivePairDeletion(ctx, claimed, livePairDeletionObservation{photo: "wrong"}, f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("missing fence accepted", err)
	}
	assertDeletionLivePair(t, r, c, "", "")
}

func TestMySQLLivePairDeletionMigrationRetainsOldUncertainty(t *testing.T) {
	f, r, c := deletionLivePairFixture(t, []string{"photo"}, true)
	if _, err := f.r.db.Exec(`ALTER TABLE album_live_pairs DROP COLUMN deletion_check_after,DROP COLUMN deletion_check_attempts,DROP COLUMN deletion_check_token,DROP COLUMN deletion_check_until`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/0047_album_deletion_live_pair_checks.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.r.db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	var after, until sql.NullTime
	var token sql.NullString
	var attempts int
	if err = f.r.db.QueryRow(`SELECT deletion_check_after,deletion_check_until,deletion_check_token,deletion_check_attempts FROM album_live_pairs WHERE pair_upload_id=?`, c.id).Scan(&after, &until, &token, &attempts); err != nil || after.Valid || until.Valid || token.Valid || attempts != 0 {
		t.Fatal("migration fabricated query", err)
	}
	assertDeletionLivePair(t, r, c, "", "")
	b, err := r.runLivePairDeletionBatch(context.Background(), func(context.Context, LivePairUpload, string) (string, error) { return "recovered", nil }, 1, func() time.Time { return f.now })
	if err != nil || b.Acknowledged != 1 {
		t.Fatal("old intent not queried", b, err)
	}
	assertDeletionLivePair(t, r, c, "recovered", "")
}

func TestCOSLivePairDeletionObservationOnlyHeadsConsumedArchive(t *testing.T) {
	u := pairUploadFixture(t)
	u.State, u.PhotoConsumed = "queued", true
	key, _ := u.key("photo", false)
	calls := 0
	s := cosFixture(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodHead || req.URL.Path != "/"+key || req.URL.RawQuery != "" {
			t.Fatal("observation escaped archive HEAD")
		}
		response := cosResponse(req, "", "archive-version")
		response.ContentLength = u.Descriptor.Photo.SizeBytes
		return response, nil
	})
	if _, err := s.ReconcileLivePairSeal(context.Background(), u, "photo"); !errors.Is(err, ErrInvalidManifest) {
		t.Fatal("ordinary verifier accepted queued pair", err)
	}
	version, err := s.observeLivePairCopy(context.Background(), u, "photo")
	if err != nil || version != "archive-version" || calls != 1 {
		t.Fatal(version, err, calls)
	}
	if _, err = s.observeLivePairCopy(context.Background(), u, "motion"); !errors.Is(err, ErrInvalidManifest) || calls != 1 {
		t.Fatal("unissued component queried", err)
	}
	u.Descriptor.Photo.SizeBytes++
	// Provider returning a different stored size is not a completed COPY receipt.
	s = cosFixture(t, func(req *http.Request) (*http.Response, error) {
		response := cosResponse(req, "", "archive-version")
		response.ContentLength = u.Descriptor.Photo.SizeBytes - 1
		return response, nil
	})
	if _, err = s.observeLivePairCopy(context.Background(), u, "photo"); !errors.Is(err, ErrLivePairNotReady) {
		t.Fatal("wrong-size receipt accepted", err)
	}
}
