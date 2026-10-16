package albums

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func deletionMultipartFixture(t *testing.T, phase string, deleted bool) (sharingFixture, Upload, deletionMultipartCheck, *liveMultipartProviderFixture) {
	t.Helper()
	f := newSharingFixture(t)
	ctx := context.Background()
	manifest := f.version.Manifest
	manifest.AssetID = uuid.NewString()
	u, err := f.uploads.Create(ctx, f.login.AccountID, uuid.NewString(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := f.repo.BeginMultipart(ctx, u.OwnerID, u.ID, "photo", *f.now)
	if err != nil {
		t.Fatal(err)
	}
	if phase != "initiating" {
		provider := "provider-photo"
		if phase == "unknown" {
			provider = ""
		}
		m, err = f.repo.ResolveMultipart(ctx, u.OwnerID, u.ID, "photo", m.IntentID, provider, *f.now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if phase == "completing" || phase == "completion-unknown" || phase == "retryable" || phase == "assembled" {
		c, _, err := f.repo.ClaimMultipartCompletion(ctx, u.OwnerID, u.ID, "photo", *f.now)
		if err != nil {
			t.Fatal(err)
		}
		state, version := phase, ""
		if phase == "completion-unknown" {
			state = "unknown"
		}
		if phase == "assembled" {
			version = "assembled-version"
		}
		if phase != "completing" {
			if _, err = f.repo.ResolveMultipartCompletion(ctx, u.OwnerID, u.ID, "photo", c.OperationID, state, version, *f.now); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := deletionMultipartCheck{account: u.OwnerID, upload: u.ID, resource: "photo", intent: m.IntentID}
	if deleted {
		d, err := sharingDeletionAccounts(t, f).requestAccountDeletion(ctx, f.session.Token, uuid.NewString(), func() time.Time { return *f.now })
		if err != nil {
			t.Fatal(err)
		}
		c.request = d.RequestID
	}
	return f, u, c, &liveMultipartProviderFixture{}
}
func TestMySQLMultipartDeletionClassifiesAndNeverRepeatsWrites(t *testing.T) {
	for _, phase := range []string{"initiating", "unknown", "ready", "retryable", "completing", "completion-unknown", "assembled"} {
		t.Run(phase, func(t *testing.T) {
			f, u, _, p := deletionMultipartFixture(t, phase, true)
			ctx := context.Background()
			clock := func() time.Time { return *f.now }
			b, err := f.repo.runMultipartDeletionBatch(ctx, p, 10, clock)
			if err != nil {
				t.Fatal(b, err)
			}
			switch phase {
			case "assembled":
				if b != (AccountMediaObservationBatch{}) {
					t.Fatal(b)
				}
			case "initiating", "unknown":
				if b.Deferred != 1 || p.finds.Load() != 1 || p.aborts.Load() != 0 {
					t.Fatal(b)
				}
				if b, err = f.repo.runMultipartDeletionBatch(ctx, p, 10, clock); err != nil || b.Acknowledged != 1 {
					t.Fatal(b, err)
				}
			default:
				if b.Acknowledged != 1 {
					t.Fatal(b)
				}
			}
			if phase == "completing" || phase == "completion-unknown" {
				if p.aborts.Load() != 0 || p.verifications.Load() != 1 {
					t.Fatal("in-flight complete aborted")
				}
			} else if phase != "assembled" && p.aborts.Load() != 1 {
				t.Fatal("ready session not stopped")
			}
			if p.initiations.Load() != 0 || p.completions.Load() != 0 {
				t.Fatal("worker created media")
			}
			if b, err = f.repo.runMultipartDeletionBatch(ctx, p, 10, clock); err != nil || b != (AccountMediaObservationBatch{}) {
				t.Fatal(b, err)
			}
			var state string
			if err = f.db.QueryRow(`SELECT state FROM album_uploads WHERE upload_id=?`, u.ID).Scan(&state); err != nil || state != string(Uploading) {
				t.Fatal("worker published/released upload", state, err)
			}
			if _, _, err = f.repo.ClaimMultipartCompletion(ctx, u.OwnerID, u.ID, "photo", *f.now); err == nil {
				t.Fatal("deletion reopened completion")
			}
		})
	}
}
func TestMySQLMultipartDeletionExcludesUnacceptedDirectAndWrongFence(t *testing.T) {
	for _, mode := range []string{"active", "direct", "fence"} {
		t.Run(mode, func(t *testing.T) {
			f, u, c, p := deletionMultipartFixture(t, "ready", mode != "active")
			ctx := context.Background()
			if mode == "direct" {
				if _, err := f.db.Exec(`UPDATE album_uploads SET direct_upload_issued=TRUE WHERE upload_id=?`, u.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "fence" {
				if _, err := f.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=DATE_ADD(deletion_requested_at,INTERVAL 1 SECOND) WHERE owner_id=?`, u.OwnerID); err != nil {
					t.Fatal(err)
				}
			}
			if b, err := f.repo.runMultipartDeletionBatch(ctx, p, 10, func() time.Time { return *f.now }); err != nil || b != (AccountMediaObservationBatch{}) {
				t.Fatal(b, err)
			}
			if _, err := f.repo.claimMultipartDeletion(ctx, c, *f.now); err == nil {
				t.Fatal("ineligible claim succeeded")
			}
			if p.aborts.Load() != 0 {
				t.Fatal("ineligible abort")
			}
		})
	}
}
func TestMySQLMultipartDeletionBackoffUnknownReadAndAbort(t *testing.T) {
	for _, phase := range []string{"unknown", "ready", "completion-unknown"} {
		t.Run(phase, func(t *testing.T) {
			f, u, _, p := deletionMultipartFixture(t, phase, true)
			ctx := context.Background()
			clock := func() time.Time { return *f.now }
			p.find = func(context.Context, string) (string, error) { return "", ErrMultipartPending }
			p.verify = func(context.Context, string, Resource) (string, error) { return "", ErrMultipartPending }
			p.abort = func(context.Context) error { return io.ErrUnexpectedEOF }
			if b, err := f.repo.runMultipartDeletionBatch(ctx, p, 1, clock); err != nil || b.Deferred != 1 {
				t.Fatal(b, err)
			}
			var receipt sql.NullTime
			if err := f.db.QueryRow(`SELECT deletion_aborted_at FROM album_multipart_intents WHERE upload_id=?`, u.ID).Scan(&receipt); err != nil || receipt.Valid {
				t.Fatal("error acknowledged", receipt, err)
			}
			*f.now = f.now.Add(59 * time.Second)
			reopened := NewMySQLUploads(f.db)
			if b, err := reopened.runMultipartDeletionBatch(ctx, p, 1, clock); err != nil || b != (AccountMediaObservationBatch{}) {
				t.Fatal(b, err)
			}
			*f.now = f.now.Add(time.Second)
			p.find = nil
			p.verify = nil
			p.abort = nil
			if b, err := reopened.runMultipartDeletionBatch(ctx, p, 1, clock); err != nil || b.Acknowledged+b.Deferred != 1 {
				t.Fatal(b, err)
			}
			if p.initiations.Load()+p.completions.Load() != 0 {
				t.Fatal("reissued unknown operation")
			}
		})
	}
}
func TestMySQLMultipartDeletionRespectsPriorReconciliationLease(t *testing.T) {
	f, u, c, p := deletionMultipartFixture(t, "unknown", false)
	ctx := context.Background()
	*f.now = f.now.Add(3 * time.Minute)
	jobs, err := f.repo.PendingReconciliations(ctx, *f.now, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	claimed, err := f.repo.ClaimReconciliation(ctx, jobs[0], *f.now)
	if err != nil {
		t.Fatal(err)
	}
	d, err := sharingDeletionAccounts(t, f).requestAccountDeletion(ctx, f.session.Token, uuid.NewString(), func() time.Time { return *f.now })
	if err != nil {
		t.Fatal(err)
	}
	c.request = d.RequestID
	if jobs, err = f.repo.PendingReconciliations(ctx, *f.now, 10); err != nil || len(jobs) != 0 {
		t.Fatal("deleted account stayed in normal queue", jobs, err)
	}
	if _, err = f.repo.ClaimReconciliation(ctx, claimed, f.now.Add(30*time.Minute)); !errors.Is(err, ErrLease) {
		t.Fatal("stale normal queue claim", err)
	}
	if _, err = f.repo.claimMultipartDeletion(ctx, c, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("overlapping lease", err)
	}
	if b, err := f.repo.runMultipartDeletionBatch(ctx, p, 10, func() time.Time { return *f.now }); err != nil || b != (AccountMediaObservationBatch{}) {
		t.Fatal(b, err)
	}
	*f.now = f.now.Add(reconciliationLease)
	if b, err := f.repo.runMultipartDeletionBatch(ctx, p, 10, func() time.Time { return *f.now }); err != nil || b.Deferred != 1 {
		t.Fatal(b, err)
	}
	m, err := f.repo.GetMultipart(ctx, u.OwnerID, u.ID, "photo")
	if err != nil || m.State != "ready" {
		t.Fatal(m, err)
	}
}
func TestMySQLMultipartDeletionLeaseScopeAndReceiptRollback(t *testing.T) {
	f, u, c, p := deletionMultipartFixture(t, "ready", true)
	ctx := context.Background()
	first, err := f.repo.claimMultipartDeletion(ctx, c, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.claimMultipartDeletion(ctx, c, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	later := f.now.Add(deletionMultipartLease)
	second, err := f.repo.claimMultipartDeletion(ctx, c, later)
	if err != nil || second.abort != first.abort {
		t.Fatal(second, err)
	}
	if _, err = f.repo.finishMultipartDeletion(ctx, first, true, later); !errors.Is(err, ErrConflict) {
		t.Fatal("stale finish accepted", err)
	}
	var receipt sql.NullTime
	if err = f.db.QueryRow(`SELECT deletion_aborted_at FROM album_multipart_intents WHERE upload_id=?`, u.ID).Scan(&receipt); err != nil || receipt.Valid {
		t.Fatal("stale receipt leaked", receipt, err)
	}
	wrong := second
	wrong.intent = uuid.NewString()
	if _, err = f.repo.finishMultipartDeletion(ctx, wrong, true, later); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	wrong = second
	wrong.account = uuid.NewString()
	if _, err = f.repo.finishMultipartDeletion(ctx, wrong, true, later); err == nil {
		t.Fatal("cross-account receipt")
	}
	wrong = second
	wrong.multipart.ProviderUploadID = "other-session"
	if _, err = f.repo.finishMultipartDeletion(ctx, wrong, true, later); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if ok, err := f.repo.finishMultipartDeletion(ctx, second, true, later); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if b, err := f.repo.runMultipartDeletionBatch(ctx, p, 1, func() time.Time { return later }); err != nil || b != (AccountMediaObservationBatch{}) {
		t.Fatal(b, err)
	}
}
func TestMySQLMultipartDeletionCancellationRetainsLateAbortReceipt(t *testing.T) {
	f, u, _, p := deletionMultipartFixture(t, "ready", true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	p.abort = func(ctx context.Context) error { close(started); <-ctx.Done(); <-release; return nil }
	done := make(chan error, 1)
	go func() {
		_, err := f.repo.runMultipartDeletionBatch(ctx, p, 1, func() time.Time { return *f.now })
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatal("returned before provider", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	var receipt sql.NullTime
	var token sql.NullString
	if err := f.db.QueryRow(`SELECT deletion_aborted_at,deletion_check_token FROM album_multipart_intents WHERE upload_id=?`, u.ID).Scan(&receipt, &token); err != nil || !receipt.Valid || token.Valid {
		t.Fatal(receipt, token, err)
	}
}
func TestMySQLMultipartDeletionPurgeGateRequiresTerminalEvidence(t *testing.T) {
	f, u, _, p := deletionMultipartFixture(t, "ready", true)
	ctx := context.Background()
	check := func() error {
		tx, err := f.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		return requireNoDirectUploads(ctx, tx, u.OwnerID, u.Manifest.AssetID)
	}
	if err := check(); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("unclosed session permitted purge", err)
	}
	if _, err := f.repo.runMultipartDeletionBatch(ctx, p, 1, func() time.Time { return *f.now }); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal("terminal session still blocked", err)
	}
}

func TestMySQLMultipartDeletionSchemaFailsBeforeProvider(t *testing.T) {
	f, _, _, p := deletionMultipartFixture(t, "ready", true)
	ctx := context.Background()
	if err := f.repo.CheckLibrarySchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`ALTER TABLE album_multipart_intents RENAME COLUMN deletion_check_after TO missing_check_after`); err != nil {
		t.Fatal(err)
	}
	defer f.db.Exec(`ALTER TABLE album_multipart_intents RENAME COLUMN missing_check_after TO deletion_check_after`)
	if err := f.repo.CheckLibrarySchema(ctx); err == nil {
		t.Fatal("incomplete schema accepted")
	}
	if _, err := f.repo.runMultipartDeletionBatch(ctx, p, 1, func() time.Time { return *f.now }); err == nil {
		t.Fatal("missing schema ran")
	}
	if p.aborts.Load()+p.finds.Load()+p.verifications.Load() != 0 {
		t.Fatal("provider used before claim")
	}
}

func TestMySQLMultipartDeletionRuntimeUsesBoundCOSAbort(t *testing.T) {
	f, u, _, _ := deletionMultipartFixture(t, "ready", true)
	calls := 0
	objects := cosFixture(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != "DELETE" || req.URL.Path != "/"+stagingKey(u, "photo") || req.URL.Query().Get("uploadId") != "provider-photo" || req.URL.Query().Get("versionId") != "" {
			t.Fatal("wrong runtime operation", req.Method, req.URL.Path)
		}
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	worker, err := NewAccountMediaObservationWorker(f.repo, objects, 10, func() time.Time { return *f.now })
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := worker.RunOnce(context.Background())
	if err != nil || b.Acknowledged != 1 || calls != 1 {
		t.Fatal(b, err, calls)
	}
	if b, err = worker.RunOnce(context.Background()); err != nil || b != (AccountMediaObservationBatch{}) || calls != 1 {
		t.Fatal(b, err, calls)
	}
}
func TestMySQLMultipartDeletionLostReceiptRetriesSameAbort(t *testing.T) {
	f, u, _, p := deletionMultipartFixture(t, "ready", true)
	ctx := context.Background()
	clock := func() time.Time { return *f.now }
	p.abort = func(context.Context) error {
		_, err := f.db.Exec(`ALTER TABLE album_multipart_intents RENAME COLUMN deletion_check_after TO missing_check_after`)
		return err
	}
	if _, err := f.repo.runMultipartDeletionBatch(ctx, p, 1, clock); err == nil {
		t.Fatal("failed receipt persistence succeeded")
	}
	if _, err := f.db.Exec(`ALTER TABLE album_multipart_intents RENAME COLUMN missing_check_after TO deletion_check_after`); err != nil {
		t.Fatal(err)
	}
	var receipt sql.NullTime
	var abort, lease sql.NullString
	if err := f.db.QueryRow(`SELECT deletion_aborted_at,deletion_abort_id,deletion_check_token FROM album_multipart_intents WHERE upload_id=?`, u.ID).Scan(&receipt, &abort, &lease); err != nil || receipt.Valid || !abort.Valid || !lease.Valid {
		t.Fatal(receipt, abort, lease, err)
	}
	p.abort = nil
	if b, err := f.repo.runMultipartDeletionBatch(ctx, p, 1, clock); err != nil || b != (AccountMediaObservationBatch{}) {
		t.Fatal(b, err)
	}
	*f.now = f.now.Add(deletionMultipartLease)
	if b, err := NewMySQLUploads(f.db).runMultipartDeletionBatch(ctx, p, 1, clock); err != nil || b.Acknowledged != 1 {
		t.Fatal(b, err)
	}
	var after string
	if err := f.db.QueryRow(`SELECT deletion_aborted_at,deletion_abort_id FROM album_multipart_intents WHERE upload_id=?`, u.ID).Scan(&receipt, &after); err != nil || !receipt.Valid || after != abort.String || p.aborts.Load() != 2 {
		t.Fatal(receipt, after, err)
	}
}
