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

func deletionVideoFixture(t *testing.T, f *pushFixture, submitted, deleted bool) (*MySQLUploads, VideoJob, videoDeletionJob) {
	t.Helper()
	u, j := videoJournalUpload(t, f.r.db)
	r := NewMySQLUploads(f.r.db)
	f.now = time.Now().UTC().Truncate(time.Microsecond)
	var err error
	if submitted {
		j, err = r.ClaimVideoJob(context.Background(), u.OwnerID, j.ID, uuid.NewString(), f.now)
		if err != nil {
			t.Fatal(err)
		}
		var fresh bool
		j, fresh, err = r.BeginVideoSubmission(context.Background(), j, journalVideoSpec(u), f.now)
		if err != nil || !fresh {
			t.Fatal("missing submission intent", err)
		}
	}
	ref := videoDeletionJob{account: u.OwnerID, id: j.ID}
	if deleted {
		deleteProtocolFixtureOwner(t, f, u.OwnerID)
		if err = f.r.db.QueryRow(`SELECT request_id FROM album_account_deletions WHERE account_id=?`, u.OwnerID).Scan(&ref.request); err != nil {
			t.Fatal(err)
		}
	}
	return r, j, ref
}

func assertDeletionVideoReceipt(t *testing.T, f *pushFixture, id string, acknowledged bool) {
	t.Helper()
	var at sql.NullTime
	var provider sql.NullString
	var succeeded sql.NullBool
	if err := f.r.db.QueryRow(`SELECT acknowledged_at,provider_job_id,provider_succeeded FROM album_deletion_video_jobs WHERE job_id=?`, id).Scan(&at, &provider, &succeeded); err != nil {
		t.Fatal(err)
	}
	if at.Valid != acknowledged || provider.Valid != acknowledged || succeeded.Valid != acknowledged {
		t.Fatal("invalid terminal receipt", at.Valid, provider.Valid, succeeded.Valid)
	}
}

func TestMySQLVideoDeletionOnlyQueuesConsumedIntents(t *testing.T) {
	f := newPushFixture(t)
	r, _, pending := deletionVideoFixture(t, f, true, true)
	_, _, unsubmitted := deletionVideoFixture(t, f, false, true)
	_, _, active := deletionVideoFixture(t, f, true, false)
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_video_jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatal("wrong deletion queue", count, err)
	}
	calls := 0
	b, err := r.runVideoDeletionBatch(context.Background(), func(ctx context.Context, j VideoJob) (VideoCleanupObservation, error) {
		calls++
		if j.ID != pending.id || j.ID == unsubmitted.id || j.ID == active.id {
			t.Fatal("observed wrong account or unissued intent")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("unbounded provider request")
		}
		return VideoCleanupObservation{ProviderJobID: "j" + strings.Repeat("a", 32), Terminal: true, Succeeded: true}, nil
	}, 10, func() time.Time { return f.now })
	if err != nil || b.Acknowledged != 1 || calls != 1 {
		t.Fatal(b, err)
	}
	assertDeletionVideoReceipt(t, f, pending.id, true)
	var state, id string
	if err := f.r.db.QueryRow(`SELECT state,provider_job_id FROM album_video_jobs WHERE job_id=?`, pending.id).Scan(&state, &id); err != nil || state != "running" || id != "j"+strings.Repeat("a", 32) {
		t.Fatal("terminal observation changed work state or lost discovered provider ID", state, err)
	}
	b, err = r.runVideoDeletionBatch(context.Background(), func(context.Context, VideoJob) (VideoCleanupObservation, error) {
		t.Fatal("acknowledged job queried again")
		return VideoCleanupObservation{}, nil
	}, 10, func() time.Time { return f.now.Add(time.Hour) })
	if err != nil || b != (videoDeletionBatch{}) {
		t.Fatal(b, err)
	}
}

func TestMySQLVideoDeletionRetriesUnknownAndNonterminalResults(t *testing.T) {
	for _, mode := range []string{"running", "missing", "error", "malformed", "wrong-provider", "success-with-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newPushFixture(t)
			r, j, ref := deletionVideoFixture(t, f, true, true)
			id := "j" + strings.Repeat("a", 32)
			if mode == "wrong-provider" {
				if _, err := r.AttachVideoSubmission(context.Background(), j, id, f.now); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			observer := func(context.Context, VideoJob) (VideoCleanupObservation, error) {
				calls++
				switch mode {
				case "running":
					return VideoCleanupObservation{ProviderJobID: id}, nil
				case "missing":
					return VideoCleanupObservation{}, nil
				case "error":
					return VideoCleanupObservation{}, errors.New("private provider error")
				case "malformed":
					return VideoCleanupObservation{ProviderJobID: "invalid", Terminal: true}, nil
				case "wrong-provider":
					return VideoCleanupObservation{ProviderJobID: "j" + strings.Repeat("b", 32), Terminal: true}, nil
				default:
					return VideoCleanupObservation{ProviderJobID: id, Terminal: true, Succeeded: true}, errors.New("unknown response")
				}
			}
			b, err := r.runVideoDeletionBatch(context.Background(), observer, 1, func() time.Time { return f.now })
			if err != nil || b.Deferred != 1 || calls != 1 {
				t.Fatal("unknown outcome accepted", b, err)
			}
			assertDeletionVideoReceipt(t, f, ref.id, false)
			b, err = r.runVideoDeletionBatch(context.Background(), observer, 1, func() time.Time { return f.now.Add(59 * time.Second) })
			if err != nil || b != (videoDeletionBatch{}) || calls != 1 {
				t.Fatal("backoff ignored", b, err)
			}
			r = NewMySQLUploads(f.r.db)
			b, err = r.runVideoDeletionBatch(context.Background(), func(context.Context, VideoJob) (VideoCleanupObservation, error) {
				return VideoCleanupObservation{ProviderJobID: id, Terminal: true}, nil
			}, 1, func() time.Time { return f.now.Add(time.Minute) })
			if err != nil || b.Acknowledged != 1 {
				t.Fatal("restart recovery failed", b, err)
			}
			assertDeletionVideoReceipt(t, f, ref.id, true)
		})
	}
}

func TestMySQLVideoDeletionLeaseAndLateProviderBinding(t *testing.T) {
	f := newPushFixture(t)
	r, j, ref := deletionVideoFixture(t, f, true, true)
	ctx := context.Background()
	first, err := r.claimVideoDeletion(ctx, ref, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.claimVideoDeletion(ctx, ref, f.now.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("live lease stolen", err)
	}
	observation := VideoCleanupObservation{ProviderJobID: "j" + strings.Repeat("a", 32), Terminal: true}
	if err = r.finishVideoDeletion(ctx, first, observation, f.now.Add(videoDeletionLease)); !errors.Is(err, ErrConflict) {
		t.Fatal("expired worker acknowledged", err)
	}
	second, err := r.claimVideoDeletion(ctx, ref, f.now.Add(videoDeletionLease))
	if err != nil || second.lease == first.lease {
		t.Fatal("abandoned lease not recovered", err)
	}
	if err = r.finishVideoDeletion(ctx, first, observation, f.now.Add(videoDeletionLease)); !errors.Is(err, ErrConflict) {
		t.Fatal("stale worker acknowledged", err)
	}
	// The active uploader can still save its late response; it cannot be overwritten.
	if _, err = r.AttachVideoSubmission(ctx, j, "j"+strings.Repeat("b", 32), f.now); err != nil {
		t.Fatal(err)
	}
	if err = r.finishVideoDeletion(ctx, second, observation, f.now.Add(videoDeletionLease)); !errors.Is(err, ErrConflict) {
		t.Fatal("different provider ID accepted", err)
	}
	assertDeletionVideoReceipt(t, f, ref.id, false)
	observation.ProviderJobID = "j" + strings.Repeat("b", 32)
	if err = r.finishVideoDeletion(ctx, second, observation, f.now.Add(videoDeletionLease+time.Second)); err != nil {
		t.Fatal(err)
	}
	assertDeletionVideoReceipt(t, f, ref.id, true)
}

func TestMySQLVideoDeletionCancellationAndFairBoundedBatch(t *testing.T) {
	f := newPushFixture(t)
	r, _, first := deletionVideoFixture(t, f, true, true)
	_, _, second := deletionVideoFixture(t, f, true, true)
	ctx, cancel := context.WithCancel(context.Background())
	b, err := r.runVideoDeletionBatch(ctx, func(context.Context, VideoJob) (VideoCleanupObservation, error) {
		cancel()
		return VideoCleanupObservation{ProviderJobID: "j" + strings.Repeat("a", 32), Terminal: true}, nil
	}, 1, func() time.Time { return f.now })
	if !errors.Is(err, context.Canceled) || b != (videoDeletionBatch{}) {
		t.Fatal("cancellation acknowledged", b, err)
	}
	assertDeletionVideoReceipt(t, f, first.id, false)
	assertDeletionVideoReceipt(t, f, second.id, false)
	calls := 0
	b, err = r.runVideoDeletionBatch(context.Background(), func(context.Context, VideoJob) (VideoCleanupObservation, error) {
		calls++
		return VideoCleanupObservation{}, nil
	}, 1, func() time.Time { return f.now })
	if err != nil || b.Deferred != 1 || calls != 1 {
		t.Fatal("leased first job starved other account", b, err)
	}
}

func TestMySQLVideoDeletionRejectsWrongRequestFenceAndSource(t *testing.T) {
	for _, mode := range []string{"request", "fence", "source", "spec"} {
		t.Run(mode, func(t *testing.T) {
			f := newPushFixture(t)
			r, _, ref := deletionVideoFixture(t, f, true, true)
			ctx := context.Background()
			if mode == "request" {
				ref.request = uuid.NewString()
			}
			if mode == "fence" {
				if _, err := f.r.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=NULL WHERE owner_id=?`, ref.account); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "source" {
				if _, err := f.r.db.Exec(`UPDATE album_asset_versions SET manifest_digest=? WHERE owner_id=?`, strings.Repeat("d", 64), ref.account); err != nil {
					t.Fatal(err)
				}
			}
			claimed, err := r.claimVideoDeletion(ctx, ref, f.now)
			if mode != "spec" {
				if err == nil {
					t.Fatal("invalid binding claimed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			claimed.video.Spec.MaxBytes--
			if err = r.finishVideoDeletion(ctx, claimed, VideoCleanupObservation{ProviderJobID: "j" + strings.Repeat("a", 32), Terminal: true}, f.now); !errors.Is(err, ErrConflict) {
				t.Fatal("changed intent acknowledged", err)
			}
			assertDeletionVideoReceipt(t, f, ref.id, false)
		})
	}
}

func TestMySQLVideoDeletionMigrationBackfillsConsumedIntents(t *testing.T) {
	f := newPushFixture(t)
	_, _, submitted := deletionVideoFixture(t, f, true, true)
	deletionVideoFixture(t, f, false, true)
	deletionVideoFixture(t, f, true, false)
	if _, err := f.r.db.Exec(`DROP TABLE album_deletion_video_jobs`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/0044_album_deletion_video_jobs.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.r.db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	var id string
	var count int
	if err = f.r.db.QueryRow(`SELECT COUNT(*),MIN(job_id) FROM album_deletion_video_jobs`).Scan(&count, &id); err != nil || count != 1 || id != submitted.id {
		t.Fatal("backfill missed or invented an intent", count, err)
	}
}

func TestMySQLVideoDeletionAcceptanceRollbackLeavesNoQueue(t *testing.T) {
	f := newPushFixture(t)
	_, j, _ := deletionVideoFixture(t, f, true, false)
	subject := uuid.NewString()
	if _, err := f.r.db.Exec(`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,?)`, j.OwnerID, appleSubjectHash(albumDevelopmentAudience, subject)); err != nil {
		t.Fatal(err)
	}
	session, _ := f.login(t, subject, f.now)
	if _, err := f.r.db.Exec(`ALTER TABLE album_share_links RENAME COLUMN revoked_at TO unavailable_revoked_at`); err != nil {
		t.Fatal(err)
	}
	_, err := f.accounts.requestAccountDeletion(context.Background(), session.Token, uuid.NewString(), func() time.Time { return f.now })
	if err == nil {
		t.Fatal("failure not injected after queue insertion")
	}
	if _, err = f.r.db.Exec(`ALTER TABLE album_share_links RENAME COLUMN unavailable_revoked_at TO revoked_at`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_video_jobs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed acceptance left queue", count, err)
	}
	var fence sql.NullTime
	if err = f.r.db.QueryRow(`SELECT deletion_requested_at FROM album_storage_accounts WHERE owner_id=?`, j.OwnerID).Scan(&fence); err != nil || fence.Valid {
		t.Fatal("failed acceptance left fence", err)
	}
	acceptAppleDeletion(t, f, session.Token)
	assertDeletionVideoReceipt(t, f, j.ID, false)
}

func TestMySQLVideoDeletionReceiptAndProviderBindingAreAtomic(t *testing.T) {
	f := newPushFixture(t)
	r, _, ref := deletionVideoFixture(t, f, true, true)
	ctx := context.Background()
	claimed, err := r.claimVideoDeletion(ctx, ref, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.r.db.Exec(`ALTER TABLE album_video_jobs RENAME COLUMN updated_at TO unavailable_updated_at`); err != nil {
		t.Fatal(err)
	}
	observation := VideoCleanupObservation{ProviderJobID: "j" + strings.Repeat("a", 32), Terminal: true}
	if err = r.finishVideoDeletion(ctx, claimed, observation, f.now); err == nil {
		t.Fatal("binding failure not injected")
	}
	if _, err = f.r.db.Exec(`ALTER TABLE album_video_jobs RENAME COLUMN unavailable_updated_at TO updated_at`); err != nil {
		t.Fatal(err)
	}
	assertDeletionVideoReceipt(t, f, ref.id, false)
	if err = r.finishVideoDeletion(ctx, claimed, observation, f.now); err != nil {
		t.Fatal("retry after rollback", err)
	}
	assertDeletionVideoReceipt(t, f, ref.id, true)
}
