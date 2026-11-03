package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func assertAccountQuiescence(t *testing.T, r *MySQLUploads, owner, request string, pending bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = lockDeletingAccount(ctx, tx, owner, request); err != nil {
		t.Fatal(err)
	}
	if _, err = lockAccount(ctx, tx, owner); err != nil {
		t.Fatal(err)
	}
	err = requireAccountMediaQuiescence(ctx, tx, owner, request)
	if pending && !errors.Is(err, ErrDerivedPending) || !pending && err != nil {
		t.Fatalf("pending=%v: %v", pending, err)
	}
}

func TestMySQLAccountQuiescenceOriginalCopyRequiresPositiveReceipt(t *testing.T) {
	for _, mode := range []string{"unissued", "observed", "legacy", "wrong-digest", "direct"} {
		t.Run(mode, func(t *testing.T) {
			f, u, c := deletionOriginalFixture(t, mode != "unissued", true)
			if mode != "unissued" {
				assertAccountQuiescence(t, f.repo, c.account, c.request, true)
				if err := f.repo.RecordOriginalSeal(context.Background(), u, "photo", "known-version"); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "legacy":
				if _, err := f.db.Exec(`UPDATE album_original_seals SET legacy=TRUE WHERE upload_id=?`, u.ID); err != nil {
					t.Fatal(err)
				}
			case "wrong-digest":
				if _, err := f.db.Exec(`UPDATE album_original_seals SET manifest_digest=? WHERE upload_id=?`, strings.Repeat("c", 64), u.ID); err != nil {
					t.Fatal(err)
				}
			case "direct":
				if _, err := f.db.Exec(`UPDATE album_uploads SET direct_upload_issued=TRUE,expires_at=? WHERE upload_id=?`, f.now.Add(-24*time.Hour), u.ID); err != nil {
					t.Fatal(err)
				}
			}
			assertAccountQuiescence(t, f.repo, c.account, c.request, mode != "unissued" && mode != "observed")
		})
	}
}

func TestMySQLAccountQuiescenceMultipartRequiresAbortOrAssembly(t *testing.T) {
	for _, phase := range []string{"initiating", "unknown", "ready", "retryable", "completing", "completion-unknown", "assembled"} {
		t.Run(phase, func(t *testing.T) {
			f, _, c, provider := deletionMultipartFixture(t, phase, true)
			assertAccountQuiescence(t, f.repo, c.account, c.request, phase != "assembled")
			for range 2 {
				if _, err := f.repo.runMultipartDeletionBatch(context.Background(), provider, 10, func() time.Time { return *f.now }); err != nil {
					t.Fatal(err)
				}
			}
			assertAccountQuiescence(t, f.repo, c.account, c.request, false)
		})
	}
}

func TestMySQLAccountQuiescencePhotoRequiresReceiptOrBoundRetirementProof(t *testing.T) {
	for _, mode := range []string{"verified", "observed", "retiring", "retired", "missing-plan", "empty-plan", "marker-only", "wrong-binding"} {
		t.Run(mode, func(t *testing.T) {
			f, r, c := deletionPhotoFixture(t, true)
			ctx := context.Background()
			assertAccountQuiescence(t, r, c.account, c.request, true)
			switch mode {
			case "verified":
				if _, err := r.CompletePhotoWrite(ctx, c.record, photoJournalReceipt(c.record.Spec), f.now); err != nil {
					t.Fatal(err)
				}
			case "observed":
				if _, err := r.runPhotoDeletionBatch(ctx, func(context.Context, PhotoWriteSpec) (string, error) { return "observed", nil }, 1, func() time.Time { return f.now }); err != nil {
					t.Fatal(err)
				}
			default:
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
				cleanup, err = r.StoreDerivedCleanupPlan(ctx, cleanup, DerivedCleanupPlan{Terminal: true, Versions: []DerivedObjectVersion{{"committed", false, c.record.Spec.SizeBytes + 1}}}, f.now)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "retired" {
					if err = r.FinishDerivedRetirement(ctx, cleanup, f.now); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "missing-plan" {
					_, err = f.r.db.Exec(`UPDATE album_derived_cleanups SET plan_json=NULL WHERE cleanup_id=?`, cleanup.ID)
				} else if mode == "empty-plan" || mode == "marker-only" {
					plan := DerivedCleanupPlan{Terminal: true}
					if mode == "marker-only" {
						plan.Versions = []DerivedObjectVersion{{"marker", true, 0}}
					}
					data, _ := json.Marshal(plan)
					_, err = f.r.db.Exec(`UPDATE album_derived_cleanups SET plan_json=? WHERE cleanup_id=?`, data, cleanup.ID)
				} else if mode == "wrong-binding" {
					_, err = f.r.db.Exec(`UPDATE album_derived_cleanups SET manifest_digest=? WHERE cleanup_id=?`, strings.Repeat("f", 64), cleanup.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			assertAccountQuiescence(t, r, c.account, c.request, mode == "missing-plan" || mode == "empty-plan" || mode == "marker-only" || mode == "wrong-binding")
		})
	}
}

func TestMySQLAccountQuiescenceVideoRequiresMatchingTerminalJob(t *testing.T) {
	for _, mode := range []string{"unsubmitted", "terminal", "missing-job", "wrong-provider"} {
		t.Run(mode, func(t *testing.T) {
			f := newPushFixture(t)
			r, _, c := deletionVideoFixture(t, f, mode != "unsubmitted", true)
			assertAccountQuiescence(t, r, c.account, c.request, mode != "unsubmitted")
			if mode != "unsubmitted" {
				if _, err := r.runVideoDeletionBatch(context.Background(), func(context.Context, VideoJob) (VideoCleanupObservation, error) {
					return VideoCleanupObservation{ProviderJobID: "j" + strings.Repeat("a", 32), Terminal: true}, nil
				}, 1, func() time.Time { return f.now }); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-job" {
				if _, err := f.r.db.Exec(`DELETE FROM album_deletion_video_jobs WHERE job_id=?`, c.id); err != nil {
					t.Fatal(err)
				}
			} else if mode == "wrong-provider" {
				if _, err := f.r.db.Exec(`UPDATE album_deletion_video_jobs SET provider_job_id=? WHERE job_id=?`, "j"+strings.Repeat("b", 32), c.id); err != nil {
					t.Fatal(err)
				}
			}
			assertAccountQuiescence(t, r, c.account, c.request, mode == "missing-job" || mode == "wrong-provider")
		})
	}
}

func TestMySQLAccountQuiescenceLivePairRequiresBothCopiesAndClosedSessions(t *testing.T) {
	f, r, c := deletionLivePairFixture(t, []string{"photo", "motion"}, true)
	ctx := context.Background()
	if _, err := r.runVideoDeletionBatch(ctx, func(_ context.Context, j VideoJob) (VideoCleanupObservation, error) {
		return VideoCleanupObservation{ProviderJobID: j.ProviderJobID, Terminal: true}, nil
	}, 10, func() time.Time { return f.now }); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"photo", "motion"} {
		assertAccountQuiescence(t, r, c.account, c.request, true)
		if _, err := r.RecordLivePairSeal(ctx, c.pair, part, "known-"+part); err != nil {
			t.Fatal(err)
		}
	}
	assertAccountQuiescence(t, r, c.account, c.request, false)
	// An independently consumed client session still blocks after both COPY receipts.
	intent, operation := uuid.NewString(), uuid.NewString()
	if _, err := f.r.db.Exec(`INSERT INTO album_live_pair_multiparts(pair_upload_id,resource_id,intent_id,state,provider_upload_id,updated_at) VALUES (?,'photo',?,'ready','live-session',?)`, c.id, intent, f.now); err != nil {
		t.Fatal(err)
	}
	assertAccountQuiescence(t, r, c.account, c.request, true)
	if _, err := f.r.db.Exec(`UPDATE album_live_pair_multiparts SET state='aborted',operation_id=? WHERE pair_upload_id=?`, operation, c.id); err != nil {
		t.Fatal(err)
	}
	assertAccountQuiescence(t, r, c.account, c.request, false)
	if _, err := f.r.db.Exec(`UPDATE album_live_pairs SET direct_upload_issued=TRUE,expires_at=? WHERE pair_upload_id=?`, f.now.Add(-24*time.Hour), c.id); err != nil {
		t.Fatal(err)
	}
	assertAccountQuiescence(t, r, c.account, c.request, true)
}

func TestMySQLAccountQuiescenceRetryConcurrencyAndNoErasure(t *testing.T) {
	f, u, c := deletionOriginalFixture(t, true, true)
	ctx := context.Background()
	ready, waiting, err := f.repo.runAccountQuiescenceBatch(ctx, 10, func() time.Time { return *f.now })
	if err != nil || ready != 0 || waiting != 1 {
		t.Fatal(ready, waiting, err)
	}
	if err = f.repo.RecordOriginalSeal(ctx, u, "photo", "late-receipt"); err != nil {
		t.Fatal(err)
	}
	ready, waiting, err = f.repo.runAccountQuiescenceBatch(ctx, 10, func() time.Time { return f.now.Add(59 * time.Second) })
	if err != nil || ready+waiting != 0 {
		t.Fatal("backoff ignored", ready, waiting, err)
	}
	var group sync.WaitGroup
	results := make(chan bool, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			r, w, e := NewMySQLUploads(f.db).checkAccountMediaQuiescence(ctx, c.account, c.request, f.now.Add(time.Minute))
			if e != nil || w {
				t.Error(r, w, e)
			}
			results <- r
		}()
	}
	group.Wait()
	close(results)
	count := 0
	for r := range results {
		if r {
			count++
		}
	}
	if count != 1 {
		t.Fatal("readiness acknowledged more than once", count)
	}
	var state string
	var receipt sql.NullTime
	var attempts, uploads, applePending int
	if err = f.db.QueryRow(`SELECT a.state,d.media_quiesced_at,d.media_check_attempts FROM album_accounts a JOIN album_account_deletions d ON d.account_id=a.account_id WHERE a.account_id=?`, c.account).Scan(&state, &receipt, &attempts); err != nil || state != "deleting" || !receipt.Valid || attempts != 2 {
		t.Fatal(state, receipt, attempts, err)
	}
	if err = f.db.QueryRow(`SELECT COUNT(*) FROM album_uploads WHERE owner_id=?`, c.account).Scan(&uploads); err != nil || uploads == 0 {
		t.Fatal("media records erased", err)
	}
	if err = f.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_apple_jobs WHERE account_id=? AND acknowledged_at IS NULL`, c.account).Scan(&applePending); err != nil || applePending == 0 {
		t.Fatal("provider revocation implied", err)
	}
	if _, _, err = f.repo.checkAccountMediaQuiescence(ctx, c.account, uuid.NewString(), f.now.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong request accepted", err)
	}
}

func TestMySQLAccountQuiescenceCurrentReadAndFenceValidation(t *testing.T) {
	f, u, c := deletionOriginalFixture(t, false, true)
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM album_original_seals WHERE upload_id=?`, u.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	// Simulate a writer's durable intent committed after an earlier snapshot.
	if _, err = f.db.Exec(`INSERT INTO album_original_seals(upload_id,resource_id,manifest_digest,consumed_at) VALUES (?,'photo',?,?)`, u.ID, u.Digest, *f.now); err != nil {
		t.Fatal(err)
	}
	if err = lockDeletingAccount(ctx, tx, c.account, c.request); err != nil {
		t.Fatal(err)
	}
	if _, err = lockAccount(ctx, tx, c.account); err != nil {
		t.Fatal(err)
	}
	if err = requireAccountMediaQuiescence(ctx, tx, c.account, c.request); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("snapshot hid write", err)
	}
	tx.Rollback()
	if _, err = f.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=? WHERE owner_id=?`, f.now.Add(time.Second), c.account); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.repo.checkAccountMediaQuiescence(ctx, c.account, c.request, f.now.Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatal("mismatched fence accepted", err)
	}
	if _, err = f.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=? WHERE owner_id=?`, *f.now, c.account); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`ALTER TABLE album_account_deletions RENAME COLUMN media_check_after TO missing_media_check_after`); err != nil {
		t.Fatal(err)
	}
	defer f.db.Exec(`ALTER TABLE album_account_deletions RENAME COLUMN missing_media_check_after TO media_check_after`)
	if err = f.repo.CheckLibrarySchema(ctx); err == nil {
		t.Fatal("missing migration accepted")
	}
	if _, _, err = f.repo.runAccountQuiescenceBatch(ctx, 10, func() time.Time { return *f.now }); err == nil {
		t.Fatal("database failure reported ready")
	}
}

func TestMySQLAccountQuiescenceRejectsConflictingMultipartReceipt(t *testing.T) {
	f, u, c, _ := deletionMultipartFixture(t, "completing", true)
	if _, err := f.db.Exec(`UPDATE album_multipart_intents SET deletion_abort_id=?,deletion_aborted_at=? WHERE upload_id=?`, uuid.NewString(), *f.now, u.ID); err != nil {
		t.Fatal(err)
	}
	assertAccountQuiescence(t, f.repo, c.account, c.request, true)
	if _, err := f.db.Exec(`UPDATE album_multipart_completions SET state='assembled',object_version='null' WHERE upload_id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	assertAccountQuiescence(t, f.repo, c.account, c.request, true)
}

func TestMySQLAccountQuiescenceScopesOwnersAndRequiresAcceptedDeletion(t *testing.T) {
	f := newPushFixture(t)
	r := NewMySQLUploads(f.r.db)
	s, _ := f.login(t, "empty-owner", f.now)
	active, _ := f.login(t, "active-owner", f.now)
	if _, _, err := r.checkAccountMediaQuiescence(context.Background(), active.AccountID, uuid.NewString(), f.now); err == nil {
		t.Fatal("active account accepted")
	}
	// An unresolved writer for another account must not block this empty account.
	_, _, foreign := deletionPhotoFixtureInDatabase(t, f)
	d, err := f.accounts.requestAccountDeletion(context.Background(), s.Token, uuid.NewString(), func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	ready, waiting, err := r.checkAccountMediaQuiescence(context.Background(), s.AccountID, d.RequestID, f.now)
	if err != nil || !ready || waiting {
		t.Fatal(ready, waiting, err)
	}
	assertAccountQuiescence(t, r, foreign.account, foreign.request, true)
	if !deletionAuthenticated(f, active.Token) {
		t.Fatal("other account revoked")
	}
}

func deletionPhotoFixtureInDatabase(t *testing.T, f *pushFixture) (*MySQLUploads, Upload, photoDeletionCheck) {
	t.Helper()
	r := NewMySQLUploads(f.r.db)
	u, candidate := photoJournalUpload(t, f.r.db)
	p, _, err := r.BeginPhotoWrite(context.Background(), u.ID, u.Digest, photoJournalSpec(u, candidate, PhotoDailyPNG), f.now)
	if err != nil {
		t.Fatal(err)
	}
	deleteProtocolFixtureOwner(t, f, u.OwnerID)
	c := photoDeletionCheck{account: u.OwnerID, attempt: p.Spec.AttemptID, record: p}
	if err = f.r.db.QueryRow(`SELECT request_id FROM album_account_deletions WHERE account_id=?`, u.OwnerID).Scan(&c.request); err != nil {
		t.Fatal(err)
	}
	return r, u, c
}
