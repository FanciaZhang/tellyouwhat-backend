package albums

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
)

func liveMultipartUnknown(t *testing.T, s *LivePairMultipartService, u LivePairUpload, p *liveMultipartProviderFixture, completing bool) {
	t.Helper()
	ctx := context.Background()
	if !completing {
		p.initiate = func(context.Context, string) (string, error) { return "", io.ErrUnexpectedEOF }
	}
	_, err := s.Initialize(ctx, u.OwnerID, u.ID, "photo")
	if !completing && !errors.Is(err, ErrMultipartPending) || completing && err != nil {
		t.Fatal(err)
	}
	if completing {
		p.complete = func(context.Context, string, string, int64) (string, error) { return "", ErrCOSMultipartUnknown }
		if _, err = s.Complete(ctx, u.OwnerID, u.ID, "photo"); !errors.Is(err, ErrMultipartPending) {
			t.Fatal(err)
		}
	}
}
func TestMySQLLiveMultipartWorkerRecoversWithoutNewWrites(t *testing.T) {
	for _, completing := range []bool{false, true} {
		t.Run(map[bool]string{false: "initialization", true: "completion"}[completing], func(t *testing.T) {
			_, s, _, u, p := liveMultipartFixture(t)
			liveMultipartUnknown(t, s, u, p, completing)
			w := &LivePairMultipartWorker{s, 10}
			b, err := w.RunOnce(context.Background())
			if err != nil || b.Resolved != 1 || b.Deferred != 0 {
				t.Fatal(b, err)
			}
			_, m, err := s.repository.liveMultipart(context.Background(), u.OwnerID, u.ID, "photo")
			expected := "ready"
			if completing {
				expected = "assembled"
			}
			if err != nil || m.State != expected {
				t.Fatal(m, err)
			}
			if b, err = w.RunOnce(context.Background()); err != nil || b != (LivePairMultipartBatch{}) {
				t.Fatal(b, err)
			}
			if p.initiations.Load() != 1 || p.aborts.Load() != 0 || p.completions.Load() != map[bool]int32{false: 0, true: 1}[completing] {
				t.Fatal("repeated provider writes")
			}
		})
	}
}
func TestMySQLLiveMultipartWorkerBackoffSurvivesRestart(t *testing.T) {
	_, s, _, u, p := liveMultipartFixture(t)
	liveMultipartUnknown(t, s, u, p, false)
	now := s.now()
	s.now = func() time.Time { return now }
	p.find = func(context.Context, string) (string, error) { return "", ErrMultipartPending }
	w := &LivePairMultipartWorker{s, 1}
	ctx := context.Background()
	b, err := w.RunOnce(ctx)
	if err != nil || b.Deferred != 1 {
		t.Fatal(b, err)
	}
	now = now.Add(59 * time.Second)
	if b, err = w.RunOnce(ctx); err != nil || b != (LivePairMultipartBatch{}) {
		t.Fatal(b, err)
	}
	reopened, err := newLivePairMultipartService(NewMySQLUploads(s.repository.db), p, s.now)
	if err != nil {
		t.Fatal(err)
	}
	w = &LivePairMultipartWorker{reopened, 1}
	now = now.Add(time.Second)
	p.find = nil
	if b, err = w.RunOnce(ctx); err != nil || b.Resolved != 1 {
		t.Fatal(b, err)
	}
	if p.finds.Load() != 2 || p.initiations.Load() != 1 {
		t.Fatal("lost backoff or duplicated initialization")
	}
	var attempts int
	var token sql.NullString
	if err = s.repository.db.QueryRow(`SELECT check_attempts,check_token FROM album_live_pair_multiparts WHERE pair_upload_id=?`, u.ID).Scan(&attempts, &token); err != nil || attempts != 2 || token.Valid {
		t.Fatal(attempts, token, err)
	}
}
func TestMySQLLiveMultipartWorkerDeletionStopsOnlyReadySessions(t *testing.T) {
	for _, phase := range []string{"active", "deleting", "completing", "fence-mismatch", "direct"} {
		t.Run(phase, func(t *testing.T) {
			f, s, _, u, p := liveMultipartFixture(t)
			ctx := context.Background()
			if phase == "completing" {
				liveMultipartUnknown(t, s, u, p, true)
			} else if _, err := s.Initialize(ctx, u.OwnerID, u.ID, "photo"); err != nil {
				t.Fatal(err)
			}
			if phase != "active" {
				deleteProtocolFixtureOwner(t, f, u.OwnerID)
			}
			if phase == "fence-mismatch" {
				if _, err := s.repository.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=DATE_ADD(deletion_requested_at,INTERVAL 1 SECOND) WHERE owner_id=?`, u.OwnerID); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "direct" {
				if _, err := s.repository.db.Exec(`UPDATE album_live_pairs SET direct_upload_issued=TRUE WHERE pair_upload_id=?`, u.ID); err != nil {
					t.Fatal(err)
				}
			}
			w := &LivePairMultipartWorker{s, 10}
			b, err := w.RunOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			resolved := phase == "deleting" || phase == "completing"
			if resolved && b.Resolved != 1 || !resolved && b != (LivePairMultipartBatch{}) {
				t.Fatal(b)
			}
			if phase == "deleting" && p.aborts.Load() != 1 || phase != "deleting" && p.aborts.Load() != 0 {
				t.Fatal("invalid abort", p.aborts.Load())
			}
			if phase == "completing" && (p.verifications.Load() != 1 || p.completions.Load() != 1) {
				t.Fatal("completion was not observed")
			}
			if b, err = w.RunOnce(ctx); err != nil || b != (LivePairMultipartBatch{}) {
				t.Fatal(b, err)
			}
		})
	}
}
func TestMySQLLiveMultipartWorkerUnknownThenAbortRetry(t *testing.T) {
	f, s, _, u, p := liveMultipartFixture(t)
	liveMultipartUnknown(t, s, u, p, false)
	deleteProtocolFixtureOwner(t, f, u.OwnerID)
	now := s.now()
	s.now = func() time.Time { return now }
	w := &LivePairMultipartWorker{s, 1}
	ctx := context.Background()
	if b, err := w.RunOnce(ctx); err != nil || b.Resolved != 1 || p.aborts.Load() != 0 {
		t.Fatal(b, err)
	}
	p.abort = func(context.Context) error { return io.ErrUnexpectedEOF }
	if b, err := w.RunOnce(ctx); err != nil || b.Deferred != 1 {
		t.Fatal(b, err)
	}
	_, before, err := s.repository.liveMultipart(ctx, u.OwnerID, u.ID, "photo")
	if err != nil || before.State != "aborting" {
		t.Fatal(before, err)
	}
	if b, err := w.RunOnce(ctx); err != nil || b != (LivePairMultipartBatch{}) {
		t.Fatal(b, err)
	}
	now = now.Add(2 * time.Minute)
	p.abort = nil
	if b, err := w.RunOnce(ctx); err != nil || b.Resolved != 1 {
		t.Fatal(b, err)
	}
	_, after, err := s.repository.liveMultipart(ctx, u.OwnerID, u.ID, "photo")
	if err != nil || after.State != "aborted" || after.ProviderID != before.ProviderID || after.OperationID != before.OperationID || p.aborts.Load() != 2 {
		t.Fatal(after, err)
	}
}
func TestMySQLLiveMultipartWorkerLeaseAndStaleFinish(t *testing.T) {
	_, s, _, u, p := liveMultipartFixture(t)
	liveMultipartUnknown(t, s, u, p, false)
	ctx := context.Background()
	_, m, err := s.repository.liveMultipart(ctx, u.OwnerID, u.ID, "photo")
	if err != nil {
		t.Fatal(err)
	}
	c := liveMultipartCheck{owner: u.OwnerID, pair: u.ID, part: "photo", intent: m.IntentID}
	first, err := s.repository.claimLiveMultipartCheck(ctx, c, s.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.repository.claimLiveMultipartCheck(ctx, c, s.now()); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	w := &LivePairMultipartWorker{s, 1}
	if b, err := w.RunOnce(ctx); err != nil || b != (LivePairMultipartBatch{}) {
		t.Fatal(b, err)
	}
	later := s.now().Add(liveMultipartCheckLease)
	second, err := s.repository.claimLiveMultipartCheck(ctx, c, later)
	if err != nil || second.attempts != 2 {
		t.Fatal(second, err)
	}
	if err = s.repository.finishLiveMultipartCheck(ctx, first, true, later); !errors.Is(err, ErrConflict) {
		t.Fatal("stale lease accepted", err)
	}
	wrong := second
	wrong.intent = uuid.NewString()
	if err = s.repository.finishLiveMultipartCheck(ctx, wrong, true, later); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong intent accepted", err)
	}
	wrong = c
	wrong.owner = uuid.NewString()
	if _, err = s.repository.claimLiveMultipartCheck(ctx, wrong, later); err == nil {
		t.Fatal("cross-owner claim accepted")
	}
	if err = s.repository.finishLiveMultipartCheck(ctx, second, false, later); err != nil {
		t.Fatal(err)
	}
	if p.finds.Load() != 0 {
		t.Fatal("leased work queried")
	}
}
func TestMySQLLiveMultipartWorkerCancellationKeepsReceiptAndWaits(t *testing.T) {
	_, s, _, u, p := liveMultipartFixture(t)
	liveMultipartUnknown(t, s, u, p, false)
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.find = func(ctx context.Context, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		<-release
		return "provider-photo", nil
	}
	w := &LivePairMultipartWorker{s, 1}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, time.Second, nil) }()
	<-started
	if b, err := w.RunOnce(context.Background()); err != nil || b != (LivePairMultipartBatch{}) {
		t.Fatal(b, err)
	}
	cancel()
	select {
	case err := <-done:
		t.Fatal("returned while provider still running", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker failed to stop")
	}
	_, m, err := s.repository.liveMultipart(context.Background(), u.OwnerID, u.ID, "photo")
	if err != nil || m.State != "ready" {
		t.Fatal(m, err)
	}
	var token sql.NullString
	if err = s.repository.db.QueryRow(`SELECT check_token FROM album_live_pair_multiparts WHERE pair_upload_id=?`, u.ID).Scan(&token); err != nil || token.Valid {
		t.Fatal(token, err)
	}
}
func TestMySQLLiveMultipartWorkerSchemaAndFailure(t *testing.T) {
	_, s, _, u, p := liveMultipartFixture(t)
	liveMultipartUnknown(t, s, u, p, false)
	ctx := context.Background()
	w := &LivePairMultipartWorker{s, 1}
	if err := w.Run(ctx, 0, nil); !errors.Is(err, ErrInvalidManifest) {
		t.Fatal(err)
	}
	if _, err := NewLivePairMultipartWorker(s.repository, nil, 1, s.now); err == nil {
		t.Fatal("missing storage")
	}
	if err := s.repository.CheckLivePairSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.repository.db.Exec(`ALTER TABLE album_live_pair_multiparts RENAME COLUMN check_after TO unavailable_after`); err != nil {
		t.Fatal(err)
	}
	defer s.repository.db.Exec(`ALTER TABLE album_live_pair_multiparts RENAME COLUMN unavailable_after TO check_after`)
	if err := s.repository.CheckLivePairSchema(ctx); err == nil {
		t.Fatal("missing migration accepted")
	}
	if _, err := w.RunOnce(ctx); err == nil {
		t.Fatal("broken queue accepted")
	}
	if p.finds.Load() != 0 {
		t.Fatal("provider called before durable claim")
	}
}
