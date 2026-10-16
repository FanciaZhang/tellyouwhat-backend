package albums

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type accountPurgeTestStorage struct {
	t                *testing.T
	f                sharingFixture
	request          string
	versions         map[string][]accountPurgeVersion
	uploads          map[string][]accountPurgeUpload
	deleted, aborted []string
	afterMutation    func()
	lostReply        bool
}

func newAccountPurgeFixture(t *testing.T) (sharingFixture, *accountPurgeTestStorage) {
	t.Helper()
	f := newSharingFixture(t)
	d, err := sharingDeletionAccounts(t, f).requestAccountDeletion(context.Background(), f.session.Token, uuid.NewString(), func() time.Time { return *f.now })
	if err != nil {
		t.Fatal(err)
	}
	ready, waiting, err := f.repo.checkAccountMediaQuiescence(context.Background(), d.AccountID, d.RequestID, *f.now)
	if err != nil || !ready || waiting {
		t.Fatal(ready, waiting, err)
	}
	return f, &accountPurgeTestStorage{t: t, f: f, request: d.RequestID, versions: map[string][]accountPurgeVersion{}, uploads: map[string][]accountPurgeUpload{}}
}

func (s *accountPurgeTestStorage) listAccountPurgeVersions(_ context.Context, owner, area string) ([]accountPurgeVersion, error) {
	if owner != s.f.login.AccountID {
		s.t.Fatal("foreign inventory")
	}
	items := s.versions[area]
	return append([]accountPurgeVersion(nil), items[:min(1000, len(items))]...), nil
}
func (s *accountPurgeTestStorage) listAccountPurgeUploads(_ context.Context, owner, area string) ([]accountPurgeUpload, error) {
	if owner != s.f.login.AccountID {
		s.t.Fatal("foreign inventory")
	}
	items := s.uploads[area]
	return append([]accountPurgeUpload(nil), items[:min(1000, len(items))]...), nil
}
func (s *accountPurgeTestStorage) deletePurgeVersion(ctx context.Context, owner string, target purgeTarget, v DerivedObjectVersion) error {
	p, err := s.f.repo.accountObjectPurge(ctx, owner)
	if err != nil {
		s.t.Fatal(err)
	}
	if p.Plan == nil || p.Action >= len(p.Plan.Versions) || p.Plan.Versions[p.Action] != (accountPurgeVersion{target, v}) {
		s.t.Fatal("deletion lacks exact durable plan")
	}
	area := accountPurgeAreas[p.Area]
	items := s.versions[area]
	remaining := make([]accountPurgeVersion, 0, len(items))
	for _, item := range items {
		if item.Target != target || item.Version.VersionID != v.VersionID {
			remaining = append(remaining, item)
		}
	}
	s.versions[area] = remaining
	s.deleted = append(s.deleted, v.VersionID)
	if s.afterMutation != nil {
		f := s.afterMutation
		s.afterMutation = nil
		f()
	}
	if s.lostReply {
		s.lostReply = false
		return ErrCOSOperation
	}
	return nil
}
func (s *accountPurgeTestStorage) abortPurgeUpload(ctx context.Context, owner string, target purgeTarget, id string) error {
	p, err := s.f.repo.accountObjectPurge(ctx, owner)
	if err != nil {
		s.t.Fatal(err)
	}
	if p.Plan == nil || p.Action >= len(p.Plan.Uploads) || p.Plan.Uploads[p.Action] != (accountPurgeUpload{target, id}) {
		s.t.Fatal("abort lacks exact durable plan")
	}
	area := accountPurgeAreas[p.Area]
	items := s.uploads[area]
	remaining := make([]accountPurgeUpload, 0, len(items))
	for _, item := range items {
		if item.Target != target || item.UploadID != id {
			remaining = append(remaining, item)
		}
	}
	s.uploads[area] = remaining
	s.aborted = append(s.aborted, id)
	return nil
}
func (s *accountPurgeTestStorage) version(area, id string, marker bool) accountPurgeVersion {
	return accountPurgeVersion{purgeTarget{accountInventoryKey(s.f.login.AccountID, area, uuid.NewString())}, DerivedObjectVersion{id, marker, 0}}
}
func (s *accountPurgeTestStorage) finish(t *testing.T, storage accountPurgeStorage) {
	t.Helper()
	for range 40 {
		if err := NewMySQLUploads(s.f.db).runAccountObjectPurgeStep(context.Background(), s.f.login.AccountID, s.request, storage, func() time.Time { return *s.f.now }); err != nil {
			t.Fatal(err)
		}
		p, err := s.f.repo.accountObjectPurge(context.Background(), s.f.login.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		if p.State == "objects_removed" {
			return
		}
	}
	t.Fatal("purge did not converge")
}

func TestMySQLAccountObjectPurgeDrainsAllNamespacesThroughCOS(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	for i := range 1003 {
		s.versions["staging"] = append(s.versions["staging"], s.version("staging", fmt.Sprintf("version-%04d", i), i%2 == 0))
	}
	for _, area := range accountPurgeAreas[1:] {
		s.versions[area] = []accountPurgeVersion{s.version(area, "opaque+&/=?#", false), s.version(area, "marker", true)}
	}
	u := accountPurgeUpload{purgeTarget{accountInventoryKey(f.login.AccountID, "staging/live-pairs", uuid.NewString())}, "orphan-upload"}
	s.uploads["staging/live-pairs"] = []accountPurgeUpload{u}
	objects := cosFixture(t, func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query()
		if req.Method == http.MethodDelete {
			if len(q) != 1 {
				t.Fatal("unscoped delete")
			}
			p, err := f.repo.accountObjectPurge(context.Background(), f.login.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if q.Has("versionId") {
				v := p.Plan.Versions[p.Action]
				if req.URL.Path != "/"+v.Target.Key || q.Get("versionId") != v.Version.VersionID {
					t.Fatal("wrong version deletion")
				}
				err = s.deletePurgeVersion(req.Context(), f.login.AccountID, v.Target, v.Version)
			} else if q.Has("uploadId") {
				v := p.Plan.Uploads[p.Action]
				if req.URL.Path != "/"+v.Target.Key || q.Get("uploadId") != v.UploadID {
					t.Fatal("wrong session deletion")
				}
				err = s.abortPurgeUpload(req.Context(), f.login.AccountID, v.Target, v.UploadID)
			} else {
				t.Fatal("versionless delete")
			}
			if err != nil {
				return nil, err
			}
			res := cosResponse(req, "", q.Get("versionId"))
			res.StatusCode = 204
			return res, nil
		}
		if req.Method != http.MethodGet || req.URL.Path != "/" || len(q) != 3 {
			t.Fatal("unexpected cloud operation")
		}
		for _, area := range accountPurgeAreas {
			prefix, _ := accountPurgePrefix(f.login.AccountID, area)
			if prefix != q.Get("prefix") {
				continue
			}
			kind, entries, nextKey, nextID := "versions", "", "", ""
			truncated := false
			if q.Has("uploads") {
				kind = "uploads"
				for _, v := range s.uploads[area] {
					entries += inventoryEntry(kind, v.Target.Key, v.UploadID, false)
				}
			} else if q.Has("versions") {
				items := s.versions[area]
				count := min(len(items), 1000)
				for _, v := range items[:count] {
					entries += inventoryEntry(kind, v.Target.Key, v.Version.VersionID, v.Version.DeleteMarker)
				}
				if len(items) > count {
					truncated = true
					nextKey = items[count-1].Target.Key
					nextID = items[count-1].Version.VersionID
				}
			} else {
				t.Fatal("wrong inventory")
			}
			return cosResponse(req, inventoryXML(kind, prefix, entries, truncated, nextKey, nextID), ""), nil
		}
		t.Fatal("foreign prefix")
		return nil, nil
	})
	// Exercise nanosecond clocks against MySQL DATETIME(6), as in production.
	*f.now = f.now.Add(123 * time.Nanosecond)
	before, reserved := photoJournalBalance(t, f.db, f.login.AccountID)
	s.finish(t, objects)
	if len(s.deleted) != 1013 || len(s.aborted) != 1 {
		t.Fatal("missing media", len(s.deleted), len(s.aborted))
	}
	used, after := photoJournalBalance(t, f.db, f.login.AccountID)
	if used != before || after != reserved {
		t.Fatal("premature quota settlement")
	}
	var state string
	var versions, pending int
	if err := f.db.QueryRow(`SELECT state FROM album_accounts WHERE account_id=?`, f.login.AccountID).Scan(&state); err != nil || state != "deleting" {
		t.Fatal(state, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_asset_versions WHERE owner_id=?`, f.login.AccountID).Scan(&versions); err != nil || versions != 1 {
		t.Fatal("metadata erased", err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_apple_jobs WHERE account_id=? AND acknowledged_at IS NULL`, f.login.AccountID).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("revocation assumed", err)
	}
}

func TestMySQLAccountObjectPurgeLostResponseAndAcknowledgementReplayExactVersion(t *testing.T) {
	for _, mode := range []string{"response", "expired-lease", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newAccountPurgeFixture(t)
			s.versions["staging"] = []accountPurgeVersion{s.version("staging", "delete-once", false)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "response":
				s.lostReply = true
			case "expired-lease":
				s.afterMutation = func() { *f.now = f.now.Add(accountPurgeLease) }
			case "cancelled":
				s.afterMutation = cancel
			}
			if err := f.repo.runAccountObjectPurgeStep(ctx, f.login.AccountID, s.request, s, func() time.Time { return *f.now }); err == nil {
				t.Fatal("missing acknowledgement accepted")
			}
			p, err := f.repo.accountObjectPurge(context.Background(), f.login.AccountID)
			if err != nil || p.Action != 0 || p.Plan == nil {
				t.Fatal("retry evidence lost", p.Action, err)
			}
			*f.now = f.now.Add(accountPurgeLease)
			s.finish(t, s)
			if len(s.deleted) != 2 || s.deleted[0] != s.deleted[1] {
				t.Fatal("retry changed version", s.deleted)
			}
		})
	}
}

func TestMySQLAccountObjectPurgeEmptyPlanIsNotAnAbsenceReceipt(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	ctx := context.Background()
	p, err := f.repo.claimAccountObjectPurge(ctx, f.login.AccountID, s.request, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.mutateAccountPurge(ctx, p, *f.now, func(_ *sql.Tx, p *accountObjectPurge) error { p.Plan = &accountPurgePlan{}; return nil }); err != nil {
		t.Fatal(err)
	}
	s.versions["staging"] = []accountPurgeVersion{s.version("staging", "found-after-crash", false)}
	*f.now = f.now.Add(accountPurgeLease)
	if err = f.repo.runAccountObjectPurgeStep(ctx, f.login.AccountID, s.request, s, func() time.Time { return *f.now }); err != nil {
		t.Fatal(err)
	}
	p, err = f.repo.accountObjectPurge(ctx, f.login.AccountID)
	if err != nil || p.Area != 0 || p.Plan != nil || len(s.deleted) != 0 {
		t.Fatal("stale emptiness advanced prefix", p.Area, err)
	}
	s.finish(t, s)
	if len(s.deleted) != 1 {
		t.Fatal("reappeared object not cleaned")
	}
}

func TestMySQLAccountObjectPurgeFinalAuditRevisitsEarlierPrefix(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	ctx := context.Background()
	for range 5 {
		if err := f.repo.runAccountObjectPurgeStep(ctx, f.login.AccountID, s.request, s, func() time.Time { return *f.now }); err != nil {
			t.Fatal(err)
		}
	}
	s.versions["originals"] = []accountPurgeVersion{s.version("originals", "late-listing", false)}
	if err := f.repo.runAccountObjectPurgeStep(ctx, f.login.AccountID, s.request, s, func() time.Time { return *f.now }); err != nil {
		t.Fatal(err)
	}
	p, err := f.repo.accountObjectPurge(ctx, f.login.AccountID)
	if err != nil || p.Area != 1 || p.State != "queued" || !p.RemovedAt.IsZero() {
		t.Fatal("incomplete account marked removed", p, err)
	}
	s.finish(t, s)
	if len(s.deleted) != 1 {
		t.Fatal("final inventory not drained")
	}
}

func TestMySQLAccountObjectPurgeLeaseAndExactPlanBinding(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	winners := make(chan accountObjectPurge, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := f.repo.claimAccountObjectPurge(ctx, f.login.AccountID, s.request, *f.now)
			if e == nil {
				winners <- p
			} else if !errors.Is(e, ErrLease) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatal("multiple lease holders")
	}
	p := <-winners
	plan := accountPurgePlan{Versions: []accountPurgeVersion{s.version("staging", "v", false)}}
	p, err := f.repo.mutateAccountPurge(ctx, p, *f.now, func(_ *sql.Tx, p *accountObjectPurge) error { p.Plan = &plan; return nil })
	if err != nil {
		t.Fatal(err)
	}
	wrong := p
	wrong.Plan = &accountPurgePlan{Versions: []accountPurgeVersion{s.version("staging", "different", false)}}
	if _, err = f.repo.mutateAccountPurge(ctx, wrong, *f.now, func(_ *sql.Tx, p *accountObjectPurge) error { p.Action++; return nil }); !errors.Is(err, ErrLease) {
		t.Fatal("different plan acknowledged", err)
	}
	*f.now = f.now.Add(accountPurgeLease)
	if _, err = f.repo.claimAccountObjectPurge(ctx, f.login.AccountID, s.request, *f.now); err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.mutateAccountPurge(ctx, p, *f.now, func(_ *sql.Tx, p *accountObjectPurge) error { p.Action++; return nil }); !errors.Is(err, ErrLease) {
		t.Fatal("old lease acknowledged", err)
	}
}

func TestMySQLAccountObjectPurgeAdmissionAndInventoryFailClosed(t *testing.T) {
	for _, mode := range []string{"missing-readiness", "wrong-fence", "wrong-request", "other-account", "foreign-version", "wrong-area", "duplicate", "mixed", "missing-schema"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newAccountPurgeFixture(t)
			ctx := context.Background()
			owner, request := f.login.AccountID, s.request
			v := s.version("staging", "v", false)
			s.versions["staging"] = []accountPurgeVersion{v}
			switch mode {
			case "missing-readiness":
				if _, err := f.db.Exec(`UPDATE album_account_deletions SET media_quiesced_at=NULL WHERE account_id=?`, owner); err != nil {
					t.Fatal(err)
				}
			case "wrong-fence":
				if _, err := f.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=? WHERE owner_id=?`, f.now.Add(time.Second), owner); err != nil {
					t.Fatal(err)
				}
			case "wrong-request":
				request = uuid.NewString()
			case "other-account":
				owner = uuid.NewString()
			case "foreign-version":
				s.versions["staging"][0].Target.Key = strings.Replace(v.Target.Key, owner, uuid.NewString(), 1)
			case "wrong-area":
				s.versions["staging"][0] = s.version("originals", "v", false)
			case "duplicate":
				s.versions["staging"] = append(s.versions["staging"], v)
			case "mixed":
				p, err := f.repo.claimAccountObjectPurge(ctx, owner, request, *f.now)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.repo.mutateAccountPurge(ctx, p, *f.now, func(_ *sql.Tx, p *accountObjectPurge) error {
					p.Plan = &accountPurgePlan{Versions: []accountPurgeVersion{v}, Uploads: []accountPurgeUpload{{v.Target, "session"}}}
					return nil
				})
				if !errors.Is(err, ErrConflict) {
					t.Fatal(err)
				}
				return
			case "missing-schema":
				if _, err := f.db.Exec(`ALTER TABLE album_account_object_purges RENAME COLUMN updated_at TO hidden_updated_at`); err != nil {
					t.Fatal(err)
				}
				defer f.db.Exec(`ALTER TABLE album_account_object_purges RENAME COLUMN hidden_updated_at TO updated_at`)
				if err := f.repo.CheckLibrarySchema(ctx); err == nil {
					t.Fatal("missing schema accepted at startup")
				}
			}
			if err := f.repo.runAccountObjectPurgeStep(ctx, owner, request, s, func() time.Time { return *f.now }); err == nil {
				t.Fatal("unsafe request accepted")
			}
			if len(s.deleted)+len(s.aborted) != 0 {
				t.Fatal("provider mutation before admission")
			}
		})
	}
}

func TestMySQLAccountObjectPurgeReadinessMarkerCannotOverrideOutstandingWrite(t *testing.T) {
	f, u, c := deletionOriginalFixture(t, true, true)
	ctx := context.Background()
	if _, err := f.db.Exec(`UPDATE album_account_deletions SET media_quiesced_at=? WHERE account_id=?`, *f.now, c.account); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.claimAccountObjectPurge(ctx, c.account, c.request, *f.now); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("readiness marker overrode unresolved COPY", err)
	}
	if err := f.repo.RecordOriginalSeal(ctx, u, "photo", "finished-copy"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.claimAccountObjectPurge(ctx, c.account, c.request, *f.now); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(accountPurgeLease)
	if _, err := f.db.Exec(`UPDATE album_uploads SET direct_upload_issued=TRUE WHERE upload_id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.claimAccountObjectPurge(ctx, c.account, c.request, *f.now); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("replay bypassed writer check", err)
	}
}
