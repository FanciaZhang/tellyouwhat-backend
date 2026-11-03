package albums

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func deletionPhotoFixture(t *testing.T, deleted bool) (*pushFixture, *MySQLUploads, photoDeletionCheck) {
	t.Helper()
	f := newPushFixture(t)
	r := NewMySQLUploads(f.r.db)
	u, candidate := photoJournalUpload(t, f.r.db)
	p, _, err := r.BeginPhotoWrite(context.Background(), u.ID, u.Digest, photoJournalSpec(u, candidate, PhotoDailyPNG), f.now)
	if err != nil {
		t.Fatal(err)
	}
	c := photoDeletionCheck{account: u.OwnerID, attempt: p.Spec.AttemptID, record: p}
	if deleted {
		deleteProtocolFixtureOwner(t, f, u.OwnerID)
		if err = f.r.db.QueryRow(`SELECT request_id FROM album_account_deletions WHERE account_id=?`, u.OwnerID).Scan(&c.request); err != nil {
			t.Fatal(err)
		}
	}
	return f, r, c
}

func assertPhotoDeletionObservation(t *testing.T, r *MySQLUploads, c photoDeletionCheck, want string) {
	t.Helper()
	var stored sql.NullString
	if err := r.db.QueryRow(`SELECT deletion_object_version FROM album_photo_writes WHERE attempt_id=?`, c.attempt).Scan(&stored); err != nil || stored.String != want {
		t.Fatal("observation mismatch", stored.String, err)
	}
}

func TestMySQLPhotoDeletionObservationCannotPublishOrSettleQuota(t *testing.T) {
	f, r, c := deletionPhotoFixture(t, true)
	before, reservedBefore := photoJournalBalance(t, f.r.db, c.account)
	ctx := context.Background()
	calls := 0
	observer := func(ctx context.Context, spec PhotoWriteSpec) (string, error) {
		calls++
		if spec != c.record.Spec {
			t.Fatal("wrong attempt queried")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("unbounded query")
		}
		return "committed-version", nil
	}
	b, err := r.runPhotoDeletionBatch(ctx, observer, 10, func() time.Time { return f.now })
	if err != nil || b.Acknowledged != 1 || calls != 1 {
		t.Fatal(b, err, calls)
	}
	assertPhotoDeletionObservation(t, r, c, "committed-version")
	p, err := r.PhotoWriteByAttempt(ctx, c.account, c.attempt)
	if err != nil || p.ObjectVersion != "" {
		t.Fatal("HEAD published a verified preview", err)
	}
	if _, err = p.Receipt(); !errors.Is(err, ErrPhotoWriteUncertain) {
		t.Fatal("HEAD became backup receipt", err)
	}
	used, reserved := photoJournalBalance(t, f.r.db, c.account)
	if used != before || reserved != reservedBefore {
		t.Fatal("HEAD settled quota")
	}
	b, err = r.runPhotoDeletionBatch(ctx, observer, 10, func() time.Time { return f.now.Add(time.Hour) })
	if err != nil || b != (photoDeletionBatch{}) || calls != 1 {
		t.Fatal("known observation repeated", b, err)
	}
	// The trusted verifier may still finish, but only for the observed version.
	receipt := photoJournalReceipt(c.record.Spec)
	if _, err = r.CompletePhotoWrite(ctx, c.record, receipt, f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting late verified version accepted", err)
	}
	receipt.VersionID = "committed-version"
	for range 2 {
		if _, err = r.CompletePhotoWrite(ctx, c.record, receipt, f.now); err != nil {
			t.Fatal("matching late verified receipt lost", err)
		}
	}
	used, reserved = photoJournalBalance(t, f.r.db, c.account)
	if used != before+c.record.Spec.SizeBytes || reserved != reservedBefore-c.record.Spec.SizeBytes {
		t.Fatal("verified receipt not settled once")
	}
}

func TestMySQLPhotoDeletionDefersUnknownAndRecoversAfterRestart(t *testing.T) {
	for _, mode := range []string{"absent", "empty", "null", "oversize", "error-with-version", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, r, c := deletionPhotoFixture(t, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			observer := func(context.Context, PhotoWriteSpec) (string, error) {
				calls++
				switch mode {
				case "absent":
					return "", ErrPhotoWriteUncertain
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
			}
			b, err := r.runPhotoDeletionBatch(ctx, observer, 1, func() time.Time { return f.now })
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if err != nil || b.Deferred != 1 {
				t.Fatal("unknown observation acknowledged", b, err)
			}
			assertPhotoDeletionObservation(t, r, c, "")
			b, err = r.runPhotoDeletionBatch(context.Background(), observer, 1, func() time.Time { return f.now.Add(59 * time.Second) })
			if err != nil || b != (photoDeletionBatch{}) || calls != 1 {
				t.Fatal("backoff or lease ignored", b, err)
			}
			b, err = NewMySQLUploads(f.r.db).runPhotoDeletionBatch(context.Background(), func(context.Context, PhotoWriteSpec) (string, error) { return "found", nil }, 1, func() time.Time { return f.now.Add(photoDeletionLease) })
			if err != nil || b.Acknowledged != 1 {
				t.Fatal("restart failed", b, err)
			}
			assertPhotoDeletionObservation(t, r, c, "found")
		})
	}
}

func TestMySQLPhotoDeletionSkipsActiveVerifiedAndRetirement(t *testing.T) {
	for _, mode := range []string{"active", "verified", "retiring", "retired"} {
		t.Run(mode, func(t *testing.T) {
			f, r, c := deletionPhotoFixture(t, mode != "active")
			ctx := context.Background()
			if mode == "verified" {
				if _, err := r.CompletePhotoWrite(ctx, c.record, photoJournalReceipt(c.record.Spec), f.now); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "retiring" || mode == "retired" {
				// Use the real durable cleanup plan; retirement timestamps alone are not evidence.
				if _, err := f.r.db.Exec(`UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`, c.account); err != nil {
					t.Fatal(err)
				}
				if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{c.account, c.record.VersionID}, f.now); err != nil {
					t.Fatal(err)
				}
				cleanup := derivedCleanupFor(t, r, c.account, "photo", c.attempt)
				cleanup, err := r.ClaimDerivedCleanup(ctx, c.account, cleanup.ID, uuid.NewString(), f.now)
				if err != nil {
					t.Fatal(err)
				}
				cleanup, err = r.StoreDerivedCleanupPlan(ctx, cleanup, DerivedCleanupPlan{Terminal: true, Versions: []DerivedObjectVersion{{"invalid-output", false, c.record.Spec.SizeBytes + 1}}}, f.now)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "retired" {
					if err = r.FinishDerivedRetirement(ctx, cleanup, f.now); err != nil {
						t.Fatal(err)
					}
				}
			}
			b, err := r.runPhotoDeletionBatch(ctx, func(context.Context, PhotoWriteSpec) (string, error) {
				t.Fatal("ineligible attempt queried")
				return "", nil
			}, 10, func() time.Time { return f.now.Add(time.Hour) })
			if err != nil || b != (photoDeletionBatch{}) {
				t.Fatal(b, err)
			}
		})
	}
}

func TestMySQLPhotoDeletionLeaseAndLateVerification(t *testing.T) {
	for _, mode := range []string{"same", "different", "query-failed"} {
		t.Run(mode, func(t *testing.T) {
			f, r, c := deletionPhotoFixture(t, true)
			ctx := context.Background()
			first, err := r.claimPhotoDeletion(ctx, c, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.claimPhotoDeletion(ctx, c, f.now.Add(time.Second)); !errors.Is(err, ErrConflict) {
				t.Fatal("lease stolen", err)
			}
			if _, err = r.finishPhotoDeletion(ctx, first, "expired", f.now.Add(photoDeletionLease)); !errors.Is(err, ErrConflict) {
				t.Fatal("expired observation saved", err)
			}
			assertPhotoDeletionObservation(t, r, c, "")
			second, err := r.claimPhotoDeletion(ctx, c, f.now.Add(photoDeletionLease))
			if err != nil || first.lease == second.lease {
				t.Fatal("lease recovery failed", err)
			}
			if _, err = r.finishPhotoDeletion(ctx, first, "stale", f.now.Add(photoDeletionLease)); !errors.Is(err, ErrConflict) {
				t.Fatal("stale observation saved", err)
			}
			receipt := photoJournalReceipt(c.record.Spec)
			if _, err = r.CompletePhotoWrite(ctx, c.record, receipt, f.now); err != nil {
				t.Fatal(err)
			}
			observed := receipt.VersionID
			if mode == "different" {
				observed = "conflict"
			}
			if mode == "query-failed" {
				observed = ""
			}
			ack, err := r.finishPhotoDeletion(ctx, second, observed, f.now.Add(photoDeletionLease+time.Second))
			if mode == "different" {
				if !errors.Is(err, ErrConflict) || ack {
					t.Fatal("conflicting observation saved", err)
				}
				assertPhotoDeletionObservation(t, r, c, "")
			} else {
				if err != nil || !ack {
					t.Fatal("late verified receipt lost", err)
				}
				assertPhotoDeletionObservation(t, r, c, receipt.VersionID)
			}
		})
	}
}

func TestMySQLPhotoDeletionBindingAndMigration(t *testing.T) {
	f, r, c := deletionPhotoFixture(t, true)
	ctx := context.Background()
	if _, err := f.r.db.Exec(`ALTER TABLE album_photo_writes DROP CHECK album_photo_deletion_version_ck,DROP COLUMN deletion_object_version,DROP COLUMN deletion_check_after,DROP COLUMN deletion_check_attempts,DROP COLUMN deletion_check_token,DROP COLUMN deletion_check_until`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/0048_album_deletion_photo_checks.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.r.db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	assertPhotoDeletionObservation(t, r, c, "")
	claimed, err := r.claimPhotoDeletion(ctx, c, f.now)
	if err != nil {
		t.Fatal("old attempt not recoverable", err)
	}
	for _, change := range []func(*photoDeletionCheck){
		func(v *photoDeletionCheck) { v.account = uuid.NewString() },
		func(v *photoDeletionCheck) { v.request = uuid.NewString() },
		func(v *photoDeletionCheck) { v.attempt = uuid.NewString() },
		func(v *photoDeletionCheck) { v.record.VersionID = uuid.NewString() },
		func(v *photoDeletionCheck) { v.record.ManifestDigest = strings.Repeat("d", 64) },
		func(v *photoDeletionCheck) { v.record.Spec.AttemptID = uuid.NewString() },
		func(v *photoDeletionCheck) { v.record.Spec.SHA256 = strings.Repeat("f", 64) },
	} {
		bad := claimed
		change(&bad)
		if _, err = r.finishPhotoDeletion(ctx, bad, "wrong", f.now); err == nil {
			t.Fatal("changed binding accepted")
		}
		assertPhotoDeletionObservation(t, r, c, "")
	}
	if _, err = f.r.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=NULL WHERE owner_id=?`, c.account); err != nil {
		t.Fatal(err)
	}
	if _, err = r.finishPhotoDeletion(ctx, claimed, "wrong", f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("missing deletion fence accepted", err)
	}
	assertPhotoDeletionObservation(t, r, c, "")
}

func TestCOSPhotoDeletionObservationDoesNotDownloadOrValidatePreviewBytes(t *testing.T) {
	spec, _ := photoWriteFixture(t)
	key, _ := spec.key()
	for _, mode := range []string{"expected-size", "invalid-preview-size", "missing", "null-version", "partial", "negative-length"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			s := cosFixture(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodHead || req.URL.Path != "/"+key || req.URL.RawQuery != "" {
					t.Fatal("observation escaped exact archive HEAD")
				}
				if mode == "missing" {
					return nil, io.ErrUnexpectedEOF
				}
				response := cosResponse(req, "", "committed-version")
				response.ContentLength = spec.SizeBytes
				if mode == "invalid-preview-size" {
					response.ContentLength = 0
				}
				if mode == "null-version" {
					response.Header.Set("x-cos-version-id", "null")
				}
				if mode == "partial" {
					response.StatusCode = 206
				}
				if mode == "negative-length" {
					response.ContentLength = -1
				}
				return response, nil
			})
			version, err := s.observePhotoDeletion(context.Background(), spec)
			if mode == "expected-size" || mode == "invalid-preview-size" {
				if err != nil || version != "committed-version" {
					t.Fatal("committed PUT not observed", version, err)
				}
			} else if !errors.Is(err, ErrPhotoWriteUncertain) {
				t.Fatal("uncertain result accepted", err)
			}
			if calls != 1 {
				t.Fatal("unexpected request count")
			}
		})
	}
}
