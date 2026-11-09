package albums

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

func erasureWorker(f sharingFixture, s accountPurgeStorage) *AccountMediaErasureWorker {
	return &AccountMediaErasureWorker{f.repo, s, 10, func() time.Time { return *f.now }}
}
func mediaSettled(t *testing.T, f sharingFixture) bool {
	t.Helper()
	var at sql.NullTime
	err := f.db.QueryRow(`SELECT media_settled_at FROM album_account_object_purges WHERE account_id=?`, f.login.AccountID).Scan(&at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return at.Valid
}

func TestMySQLAccountMediaErasureWorkerResumesAndSettles(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	ctx := context.Background()
	for _, area := range accountPurgeAreas {
		s.versions[area] = []accountPurgeVersion{s.version(area, "original-version", false), s.version(area, "deleted-marker", true)}
	}
	w := erasureWorker(f, s)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	advanced, settled := 0, 0
	for range 30 {
		// A new instance on every pass exercises persisted progress, not in-memory state.
		w = erasureWorker(f, s)
		b, err := w.RunOnce(ctx)
		if err != nil || b.Deferred+b.Contended != 0 {
			t.Fatal(b, err)
		}
		advanced += b.Advanced
		settled += b.Settled
		*f.now = f.now.Add(time.Second)
		if settled > 0 {
			break
		}
	}
	if settled != 1 || advanced == 0 || !mediaSettled(t, f) || len(s.deleted) != 12 {
		t.Fatal(advanced, settled, len(s.deleted))
	}
	var assets, used, reserved int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_asset_versions WHERE owner_id=?`, f.login.AccountID).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, f.login.AccountID).Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	if assets+used+reserved != 0 {
		t.Fatal("media settlement did not commit")
	}
	var state string
	if err := f.db.QueryRow(`SELECT state FROM album_accounts WHERE account_id=?`, f.login.AccountID).Scan(&state); err != nil || state != "deleting" {
		t.Fatal("media receipt implied full erasure", err)
	}
	w.storage = &erasureFailStorage{accountPurgeStorage: s, t: t, unexpected: true}
	b, err := w.RunOnce(ctx)
	if err != nil || b != (AccountMediaErasureBatch{}) {
		t.Fatal("settled account selected", b, err)
	}
	run, cancel := context.WithCancel(ctx)
	reports := 0
	err = w.Run(run, time.Second, func(b AccountMediaErasureBatch, ok bool) {
		reports++
		if !ok || b != (AccountMediaErasureBatch{}) {
			t.Error(b, ok)
		}
		cancel()
	})
	if !errors.Is(err, context.Canceled) || reports != 1 {
		t.Fatal(err, reports)
	}
}

type erasureFailStorage struct {
	accountPurgeStorage
	t          *testing.T
	unexpected bool
	calls      int
	cancel     context.CancelFunc
}

func (s *erasureFailStorage) listAccountPurgeUploads(ctx context.Context, owner, area string) ([]accountPurgeUpload, error) {
	s.calls++
	if s.unexpected {
		s.t.Fatal("unexpected cloud request")
	}
	if s.cancel != nil {
		s.cancel()
	}
	return nil, ErrCOSOperation
}

func TestMySQLAccountMediaErasureWorkerFailureBackoffAndCancellation(t *testing.T) {
	for _, mode := range []string{"objects", "settlement", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newAccountPurgeFixture(t)
			ctx := context.Background()
			if mode == "settlement" {
				s.finish(t, s)
			}
			fail := &erasureFailStorage{accountPurgeStorage: s, t: t}
			w := erasureWorker(f, fail)
			var cancel context.CancelFunc
			if mode == "cancel" {
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				fail.cancel = cancel
			}
			b, err := w.RunOnce(ctx)
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if err != nil || b.Deferred != 1 {
				t.Fatal(b, err)
			}
			if mediaSettled(t, f) {
				t.Fatal("error completed media erasure")
			}
			var assets int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_asset_versions WHERE owner_id=?`, f.login.AccountID).Scan(&assets); err != nil || assets == 0 {
				t.Fatal("index lost", err)
			}
			for range 2 {
				b, err = erasureWorker(f, fail).RunOnce(context.Background())
				if err != nil || b != (AccountMediaErasureBatch{}) {
					t.Fatal("failed work immediately retried", b, err)
				}
			}
			if fail.calls != 1 {
				t.Fatal("provider hammered", fail.calls)
			}
			*f.now = f.now.Add(accountPurgeLease + time.Second)
			for range 20 {
				b, err = erasureWorker(f, s).RunOnce(context.Background())
				if err != nil || b.Deferred != 0 {
					t.Fatal(b, err)
				}
				*f.now = f.now.Add(time.Second)
				if b.Settled == 1 {
					break
				}
			}
			if !mediaSettled(t, f) {
				t.Fatal("failed work did not recover")
			}
		})
	}
}

func TestMySQLAccountMediaErasureWorkerClaimFencesAndStaleFinish(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	w := erasureWorker(f, s)
	ctx := context.Background()
	var wg sync.WaitGroup
	type result struct {
		until time.Time
		err   error
	}
	results := make(chan result, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			until, err := w.claim(ctx, f.login.AccountID, s.request)
			results <- result{until, err}
		}()
	}
	wg.Wait()
	close(results)
	var first time.Time
	won, lost := 0, 0
	for r := range results {
		if r.err == nil {
			won++
			first = r.until
		} else if errors.Is(r.err, ErrLease) {
			lost++
		} else {
			t.Fatal(r.err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatal(won, lost)
	}
	*f.now = first.Add(time.Second)
	second, err := w.claim(ctx, f.login.AccountID, s.request)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.finish(ctx, f.login.AccountID, s.request, first, false); err != nil {
		t.Fatal(err)
	}
	var actual time.Time
	if err = f.db.QueryRow(`SELECT media_purge_after FROM album_account_deletions WHERE account_id=?`, f.login.AccountID).Scan(&actual); err != nil || !actual.Equal(second) {
		t.Fatal("stale worker rescheduled newer claim", actual, second, err)
	}
	if _, err = w.claim(ctx, f.login.AccountID, s.request); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
}

func TestMySQLAccountMediaErasureWorkerIgnoresUnreadyAndRejectsMissingSchema(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	w := erasureWorker(f, &erasureFailStorage{accountPurgeStorage: s, t: t, unexpected: true})
	ctx := context.Background()
	if _, err := f.db.Exec(`UPDATE album_account_deletions SET media_quiesced_at=NULL WHERE account_id=?`, f.login.AccountID); err != nil {
		t.Fatal(err)
	}
	if b, err := w.RunOnce(ctx); err != nil || b != (AccountMediaErasureBatch{}) {
		t.Fatal(b, err)
	}
	if _, err := f.db.Exec(`ALTER TABLE album_account_deletions RENAME COLUMN media_purge_after TO hidden_media_purge_after`); err != nil {
		t.Fatal(err)
	}
	if err := w.Check(ctx); err == nil {
		t.Fatal("missing schema accepted")
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	err := w.Run(run, time.Second, func(_ AccountMediaErasureBatch, ok bool) {
		if ok {
			t.Error("missing schema reported available")
		}
		cancel()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMySQLAccountMediaErasureWorkerLostDeleteReplyRetriesSameVersion(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	s.versions[accountPurgeAreas[0]] = []accountPurgeVersion{s.version(accountPurgeAreas[0], "lost-response-version", false)}
	s.lostReply = true
	w := erasureWorker(f, s)
	b, err := w.RunOnce(context.Background())
	if err != nil || b.Deferred != 1 || len(s.deleted) != 1 {
		t.Fatal(b, err, s.deleted)
	}
	p, err := f.repo.accountObjectPurge(context.Background(), f.login.AccountID)
	if err != nil || p.Action != 0 || p.Plan == nil || p.Plan.Versions[0].Version.VersionID != "lost-response-version" {
		t.Fatal("lost plan", p, err)
	}
	*f.now = f.now.Add(accountPurgeLease + time.Second)
	for range 20 {
		b, err = erasureWorker(f, s).RunOnce(context.Background())
		if err != nil || b.Deferred != 0 {
			t.Fatal(b, err)
		}
		*f.now = f.now.Add(time.Second)
		if b.Settled == 1 {
			break
		}
	}
	if !mediaSettled(t, f) || len(s.deleted) != 2 || s.deleted[0] != s.deleted[1] {
		t.Fatal("retry did not target identical version", s.deleted)
	}
}

type erasureSelectiveStorage struct {
	t                     *testing.T
	failOwner, otherOwner string
	failed                int
}

func (s *erasureSelectiveStorage) listAccountPurgeUploads(_ context.Context, owner, area string) ([]accountPurgeUpload, error) {
	if owner == s.failOwner {
		s.failed++
		return nil, ErrCOSOperation
	}
	if owner != s.otherOwner {
		s.t.Fatal("active or unready account selected")
	}
	return nil, nil
}
func (s *erasureSelectiveStorage) listAccountPurgeVersions(_ context.Context, owner, area string) ([]accountPurgeVersion, error) {
	if owner != s.otherOwner {
		s.t.Fatal("foreign inventory")
	}
	return nil, nil
}
func (s *erasureSelectiveStorage) deletePurgeVersion(context.Context, string, purgeTarget, DerivedObjectVersion) error {
	s.t.Fatal("unexpected delete")
	return nil
}
func (s *erasureSelectiveStorage) abortPurgeUpload(context.Context, string, purgeTarget, string) error {
	s.t.Fatal("unexpected abort")
	return nil
}

func TestMySQLAccountMediaErasureWorkerFailedAccountDoesNotStarveOthers(t *testing.T) {
	f := newPushFixture(t)
	repo := NewMySQLUploads(f.r.db)
	ctx := context.Background()
	var deleted []accountDeletionRequest
	for _, subject := range []string{"fails", "healthy", "unready", "active"} {
		session, _ := f.login(t, subject, f.now)
		if subject == "active" {
			continue
		}
		d := acceptAppleDeletion(t, f, session.Token)
		deleted = append(deleted, d)
		if subject != "unready" {
			if ready, waiting, err := repo.checkAccountMediaQuiescence(ctx, d.AccountID, d.RequestID, f.now); err != nil || !ready || waiting {
				t.Fatal(ready, waiting, err)
			}
		}
		f.now = f.now.Add(time.Second)
	}
	storage := &erasureSelectiveStorage{t: t, failOwner: deleted[0].AccountID, otherOwner: deleted[1].AccountID}
	w := &AccountMediaErasureWorker{repo, storage, 1, func() time.Time { return f.now }}
	b, err := w.RunOnce(ctx)
	if err != nil || b.Deferred != 1 || storage.failed != 1 {
		t.Fatal(b, err, storage.failed)
	}
	total := 0
	for range 10 {
		b, err = w.RunOnce(ctx)
		if err != nil || b.Deferred != 0 {
			t.Fatal(b, err)
		}
		total += b.Settled
		f.now = f.now.Add(time.Second)
	}
	if total != 1 || storage.failed != 1 {
		t.Fatal("failed account blocked other erasure", total, storage.failed)
	}
	var rows int
	if err = f.r.db.QueryRow(`SELECT COUNT(*) FROM album_account_object_purges WHERE media_settled_at IS NOT NULL`).Scan(&rows); err != nil || rows != 1 {
		t.Fatal(rows, err)
	}
}
