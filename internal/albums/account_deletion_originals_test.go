package albums

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func deletionOriginalFixture(t *testing.T, consume, deleted bool) (sharingFixture, Upload, originalDeletionCheck) {
	t.Helper()
	f := newSharingFixture(t)
	u := lifecycleUpload(t, f, "deletion-original")
	ctx := context.Background()
	var err error
	if _, err = f.repo.Submit(ctx, u.OwnerID, u.ID, *f.now); err != nil {
		t.Fatal(err)
	}
	u, err = f.repo.Claim(ctx, u.OwnerID, u.ID, uuid.NewString(), *f.now, f.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	c := originalDeletionCheck{account: u.OwnerID, upload: u.ID, resource: "photo", digest: u.Digest}
	if consume {
		if _, fresh, err := f.repo.BeginOriginalSeal(ctx, u, "photo", *f.now); err != nil || !fresh {
			t.Fatal("COPY intent not consumed", err)
		}
		if err = f.db.QueryRow(`SELECT consumed_at FROM album_original_seals WHERE upload_id=? AND resource_id='photo'`, u.ID).Scan(&c.consumed); err != nil {
			t.Fatal(err)
		}
	}
	if deleted {
		d, err := sharingDeletionAccounts(t, f).requestAccountDeletion(ctx, f.session.Token, uuid.NewString(), func() time.Time { return *f.now })
		if err != nil {
			t.Fatal(err)
		}
		c.request = d.RequestID
	}
	return f, u, c
}

func assertDeletionOriginalVersion(t *testing.T, f sharingFixture, u Upload, want string) {
	t.Helper()
	version, consumed, err := f.repo.OriginalSeal(context.Background(), u, "photo")
	if err != nil || !consumed || version != want {
		t.Fatal("COPY receipt mismatch", version, consumed, err)
	}
}

func TestMySQLOriginalDeletionRecoveryNeverPublishesOrRepeatsCopy(t *testing.T) {
	f, u, c := deletionOriginalFixture(t, true, true)
	calls := 0
	observer := func(ctx context.Context, key string) (string, error) {
		calls++
		if key != sealedKey(u, "photo") {
			t.Fatal("wrong account or namespace")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("unbounded query")
		}
		return "recovered-copy-version", nil
	}
	b, err := f.repo.runOriginalDeletionBatch(context.Background(), observer, 10, func() time.Time { return *f.now })
	if err != nil || b.Acknowledged != 1 || calls != 1 {
		t.Fatal(b, err)
	}
	assertDeletionOriginalVersion(t, f, u, "recovered-copy-version")
	var state string
	if err = f.db.QueryRow(`SELECT state FROM album_uploads WHERE upload_id=?`, u.ID).Scan(&state); err != nil || state != string(Verifying) {
		t.Fatal("observation published backup", state, err)
	}
	b, err = f.repo.runOriginalDeletionBatch(context.Background(), observer, 10, func() time.Time { return f.now.Add(time.Hour) })
	if err != nil || b != (originalDeletionBatch{}) || calls != 1 {
		t.Fatal("repeated completed observation", b, err)
	}
	if _, err = f.repo.claimOriginalDeletion(context.Background(), c, f.now.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatal("positive receipt reclaimed", err)
	}
}

func TestMySQLOriginalDeletionDefersMissingInvalidAndFailedObservation(t *testing.T) {
	for _, mode := range []string{"absent", "empty", "null", "oversize", "error-with-version"} {
		t.Run(mode, func(t *testing.T) {
			f, u, _ := deletionOriginalFixture(t, true, true)
			calls := 0
			observer := func(context.Context, string) (string, error) {
				calls++
				switch mode {
				case "absent":
					return "", ErrCOSOperation
				case "empty":
					return "", nil
				case "null":
					return "null", nil
				case "oversize":
					return strings.Repeat("v", 1025), nil
				default:
					return "uncertain-version", errors.New("private provider failure")
				}
			}
			b, err := f.repo.runOriginalDeletionBatch(context.Background(), observer, 1, func() time.Time { return *f.now })
			if err != nil || b.Deferred != 1 || calls != 1 {
				t.Fatal("uncertain COPY closed", b, err)
			}
			assertDeletionOriginalVersion(t, f, u, "")
			b, err = f.repo.runOriginalDeletionBatch(context.Background(), observer, 1, func() time.Time { return f.now.Add(59 * time.Second) })
			if err != nil || b != (originalDeletionBatch{}) || calls != 1 {
				t.Fatal("backoff ignored", b, err)
			}
			reopened := NewMySQLUploads(f.db)
			b, err = reopened.runOriginalDeletionBatch(context.Background(), func(context.Context, string) (string, error) { return "found", nil }, 1, func() time.Time { return f.now.Add(time.Minute) })
			if err != nil || b.Acknowledged != 1 {
				t.Fatal("restart did not recover receipt", b, err)
			}
			assertDeletionOriginalVersion(t, f, u, "found")
		})
	}
}

func TestMySQLOriginalDeletionSkipsUnissuedActiveLegacyAndKnownReceipts(t *testing.T) {
	for _, mode := range []string{"unissued", "active", "legacy", "known"} {
		t.Run(mode, func(t *testing.T) {
			f, u, c := deletionOriginalFixture(t, mode != "unissued", mode != "active")
			if mode == "legacy" {
				if _, err := f.db.Exec(`UPDATE album_original_seals SET legacy=TRUE WHERE upload_id=?`, u.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "known" {
				if err := f.repo.RecordOriginalSeal(context.Background(), u, "photo", "existing"); err != nil {
					t.Fatal(err)
				}
			}
			b, err := f.repo.runOriginalDeletionBatch(context.Background(), func(context.Context, string) (string, error) { t.Fatal("ineligible intent queried"); return "", nil }, 10, func() time.Time { return f.now.Add(time.Hour) })
			if err != nil || b != (originalDeletionBatch{}) {
				t.Fatal(b, err)
			}
			if mode == "legacy" {
				if _, err = f.repo.claimOriginalDeletion(context.Background(), c, *f.now); !errors.Is(err, ErrConflict) {
					t.Fatal("legacy intent treated as once-only", err)
				}
				assertDeletionOriginalVersion(t, f, u, "")
			}
		})
	}
}

func TestMySQLOriginalDeletionLeaseAndLateReceipts(t *testing.T) {
	for _, mode := range []string{"same", "different", "late-after-query-failure"} {
		t.Run(mode, func(t *testing.T) {
			f, u, c := deletionOriginalFixture(t, true, true)
			ctx := context.Background()
			first, err := f.repo.claimOriginalDeletion(ctx, c, *f.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.repo.claimOriginalDeletion(ctx, c, f.now.Add(time.Second)); !errors.Is(err, ErrConflict) {
				t.Fatal("lease stolen", err)
			}
			if _, err = f.repo.finishOriginalDeletion(ctx, first, "observed", f.now.Add(originalDeletionLease)); !errors.Is(err, ErrConflict) {
				t.Fatal("expired receipt accepted", err)
			}
			second, err := f.repo.claimOriginalDeletion(ctx, c, f.now.Add(originalDeletionLease))
			if err != nil || first.lease == second.lease {
				t.Fatal("lease not recovered", err)
			}
			if _, err = f.repo.finishOriginalDeletion(ctx, first, "observed", f.now.Add(originalDeletionLease)); !errors.Is(err, ErrConflict) {
				t.Fatal("stale receipt accepted", err)
			}
			if err = f.repo.RecordOriginalSeal(ctx, u, "photo", "real-late"); err != nil {
				t.Fatal(err)
			}
			observed := "real-late"
			if mode == "different" {
				observed = "other"
			}
			if mode == "late-after-query-failure" {
				observed = ""
			}
			ack, err := f.repo.finishOriginalDeletion(ctx, second, observed, f.now.Add(originalDeletionLease+time.Second))
			if mode == "different" {
				if !errors.Is(err, ErrConflict) || ack {
					t.Fatal("conflicting receipt overwrote original", err)
				}
			} else if err != nil || !ack {
				t.Fatal("late positive receipt lost", err)
			}
			assertDeletionOriginalVersion(t, f, u, "real-late")
		})
	}
}

func TestMySQLOriginalDeletionRejectsChangedBindingAndCancelledObservation(t *testing.T) {
	for _, mode := range []string{"request", "resource", "digest", "consumed", "fence", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, u, c := deletionOriginalFixture(t, true, true)
			ctx := context.Background()
			switch mode {
			case "request":
				c.request = uuid.NewString()
			case "resource":
				c.resource = "unissued"
			case "digest":
				c.digest = strings.Repeat("d", 64)
			case "consumed":
				c.consumed = c.consumed.Add(time.Microsecond)
			case "fence":
				if _, err := f.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=NULL WHERE owner_id=?`, u.OwnerID); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				ctx, cancel := context.WithCancel(ctx)
				_, err := f.repo.runOriginalDeletionBatch(ctx, func(context.Context, string) (string, error) { cancel(); return "uncommitted", nil }, 1, func() time.Time { return *f.now })
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancelled observation committed", err)
				}
				assertDeletionOriginalVersion(t, f, u, "")
				return
			}
			if _, err := f.repo.claimOriginalDeletion(ctx, c, *f.now); err == nil {
				t.Fatal("invalid intent claimed")
			}
			assertDeletionOriginalVersion(t, f, u, "")
		})
	}
}

func TestMySQLOriginalDeletionMigrationPreservesUncertainIntents(t *testing.T) {
	f, u, _ := deletionOriginalFixture(t, true, true)
	if _, err := f.db.Exec(`ALTER TABLE album_original_seals DROP COLUMN deletion_check_after,DROP COLUMN deletion_check_attempts,DROP COLUMN deletion_check_token,DROP COLUMN deletion_check_until`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/0045_album_deletion_original_checks.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	assertDeletionOriginalVersion(t, f, u, "")
	var after, until sql.NullTime
	var token sql.NullString
	var attempts int
	if err = f.db.QueryRow(`SELECT deletion_check_after,deletion_check_until,deletion_check_token,deletion_check_attempts FROM album_original_seals WHERE upload_id=? AND resource_id='photo'`, u.ID).Scan(&after, &until, &token, &attempts); err != nil || after.Valid || until.Valid || token.Valid || attempts != 0 {
		t.Fatal("migration fabricated a query or receipt", err)
	}
	b, err := f.repo.runOriginalDeletionBatch(context.Background(), func(context.Context, string) (string, error) { return "recovered", nil }, 1, func() time.Time { return *f.now })
	if err != nil || b.Acknowledged != 1 {
		t.Fatal("old consumed intent not recoverable", b, err)
	}
}
