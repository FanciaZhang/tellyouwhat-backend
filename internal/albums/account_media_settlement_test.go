package albums

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func readyAccountMediaSettlement(t *testing.T, f sharingFixture, request string) *accountPurgeTestStorage {
	t.Helper()
	ready, _, err := f.repo.checkAccountMediaQuiescence(context.Background(), f.login.AccountID, request, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		var stored sql.NullTime
		if err = f.db.QueryRow(`SELECT media_quiesced_at FROM album_account_deletions WHERE account_id=?`, f.login.AccountID).Scan(&stored); err != nil || !stored.Valid {
			t.Fatal("fixture still has unresolved writers", err)
		}
	}
	s := &accountPurgeTestStorage{t: t, f: f, request: request, versions: map[string][]accountPurgeVersion{}, uploads: map[string][]accountPurgeUpload{}}
	s.finish(t, s)
	return s
}

func mediaSettlementCounts(t *testing.T, db *sql.DB, owner string) map[string]int {
	t.Helper()
	queries := map[string]string{}
	for _, table := range []string{"album_share_links", "album_object_purges", "album_asset_operations", "album_library_changes", "album_live_pairs", "album_derived_cleanups", "album_derived_finalizations", "album_photo_jobs", "album_photo_writes", "album_video_jobs", "album_uploads", "album_asset_versions", "album_asset_lifecycle"} {
		queries[table] = `SELECT COUNT(*) FROM ` + table + ` WHERE owner_id=?`
	}
	queries["album_share_claims"] = `SELECT COUNT(*) FROM album_share_claims c JOIN album_share_links s ON s.share_id=c.share_id WHERE s.owner_id=?`
	queries["album_live_pair_multiparts"] = `SELECT COUNT(*) FROM album_live_pair_multiparts m JOIN album_live_pairs p ON p.pair_upload_id=m.pair_upload_id WHERE p.owner_id=?`
	queries["album_deletion_video_jobs"] = `SELECT COUNT(*) FROM album_deletion_video_jobs WHERE account_id=?`
	for _, table := range []string{"album_multipart_completions", "album_multipart_intents", "album_original_seals"} {
		queries[table] = `SELECT COUNT(*) FROM ` + table + ` m JOIN album_uploads u ON u.upload_id=m.upload_id WHERE u.owner_id=?`
	}
	counts := map[string]int{}
	for table, query := range queries {
		var n int
		if err := db.QueryRow(query, owner).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[table] = n
	}
	return counts
}

func TestMySQLAccountMediaSettlementAllMediaAndForeignAccountPreserved(t *testing.T) {
	for _, kind := range []string{"photo", "video", "live-pair", "unpublished-multipart"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var f sharingFixture
			var request string
			var late func() error
			if kind == "unpublished-multipart" {
				var c deletionMultipartCheck
				f, _, c, _ = deletionMultipartFixture(t, "assembled", true)
				request = c.request
			} else {
				var pf *pushFixture
				var r *MySQLUploads
				var owner string
				switch kind {
				case "photo":
					var c photoDeletionCheck
					pf, r, c = deletionPhotoFixture(t, true)
					owner, request = c.account, c.request
					late = func() error {
						_, e := r.CompletePhotoWrite(ctx, c.record, photoJournalReceipt(c.record.Spec), pf.now)
						return e
					}
					if err := late(); err != nil {
						t.Fatal(err)
					}
					if _, err := pf.r.db.Exec(`UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`, owner); err != nil {
						t.Fatal(err)
					}
					if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{owner, c.record.VersionID}, pf.now); err != nil {
						t.Fatal(err)
					}
				case "video":
					pf = newPushFixture(t)
					var c videoDeletionJob
					r, _, c = deletionVideoFixture(t, pf, true, true)
					owner, request = c.account, c.request
					if _, err := r.runVideoDeletionBatch(ctx, func(context.Context, VideoJob) (VideoCleanupObservation, error) {
						return VideoCleanupObservation{ProviderJobID: "j" + strings.Repeat("a", 32), Terminal: true}, nil
					}, 10, func() time.Time { return pf.now }); err != nil {
						t.Fatal(err)
					}
				case "live-pair":
					var c livePairDeletionCheck
					pf, r, c = deletionLivePairFixture(t, []string{"photo", "motion"}, true)
					owner, request = c.account, c.request
					for _, part := range []string{"photo", "motion"} {
						if _, err := r.RecordLivePairSeal(ctx, c.pair, part, "known-"+part); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := r.runVideoDeletionBatch(ctx, func(_ context.Context, j VideoJob) (VideoCleanupObservation, error) {
						return VideoCleanupObservation{ProviderJobID: j.ProviderJobID, Terminal: true}, nil
					}, 10, func() time.Time { return pf.now }); err != nil {
						t.Fatal(err)
					}
					if _, err := pf.r.db.Exec(`INSERT INTO album_live_pair_multiparts(pair_upload_id,resource_id,intent_id,state,provider_upload_id,operation_id,object_version,updated_at) VALUES (?,'photo',?,'assembled','provider',?,'assembled-version',?)`, c.id, uuid.NewString(), uuid.NewString(), pf.now); err != nil {
						t.Fatal(err)
					}
				}
				f = sharingFixture{db: pf.r.db, repo: r, login: AccountLogin{AccountID: owner}, now: &pf.now}
			}
			s := readyAccountMediaSettlement(t, f, request)
			foreign, _ := photoJournalUpload(t, f.db)
			before := mediaSettlementCounts(t, f.db, foreign.OwnerID)
			foreignUsed, foreignReserved := photoJournalBalance(t, f.db, foreign.OwnerID)
			own := mediaSettlementCounts(t, f.db, f.login.AccountID)
			if own["album_uploads"] == 0 {
				t.Fatal("fixture lacks media")
			}
			if _, err := f.repo.settleAccountMediaPurge(ctx, f.login.AccountID, request, s, func() time.Time { return *f.now }); err != nil {
				t.Fatal(err)
			}
			for table, n := range mediaSettlementCounts(t, f.db, f.login.AccountID) {
				if n != 0 {
					t.Error("retained media", table, n)
				}
			}
			for table, n := range mediaSettlementCounts(t, f.db, foreign.OwnerID) {
				if n != before[table] {
					t.Error("other owner changed", table)
				}
			}
			afterForeignUsed, afterForeignReserved := photoJournalBalance(t, f.db, foreign.OwnerID)
			if afterForeignUsed != foreignUsed || afterForeignReserved != foreignReserved {
				t.Fatal("other owner quota changed")
			}
			used, reserved := photoJournalBalance(t, f.db, f.login.AccountID)
			if used+reserved != 0 {
				t.Fatal("quota not cleared", used, reserved)
			}
			if late != nil && late() == nil {
				t.Fatal("late receipt recreated erased media")
			}
			var state string
			var pending int
			if err := f.db.QueryRow(`SELECT state FROM album_accounts WHERE account_id=?`, f.login.AccountID).Scan(&state); err != nil || state != "deleting" {
				t.Fatal("account marked fully erased", err)
			}
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_apple_jobs WHERE account_id=? AND acknowledged_at IS NULL`, f.login.AccountID).Scan(&pending); err != nil || pending == 0 {
				t.Fatal("provider cleanup lost", err)
			}
		})
	}
}

func TestMySQLAccountMediaSettlementSharesReplayAndTransactionRollback(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	_, token := f.create(t, "", 2)
	if _, err := f.service.Claim(ctx, token, "", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	d, err := sharingDeletionAccounts(t, f).requestAccountDeletion(ctx, f.session.Token, uuid.NewString(), func() time.Time { return *f.now })
	if err != nil {
		t.Fatal(err)
	}
	s := readyAccountMediaSettlement(t, f, d.RequestID)
	before := mediaSettlementCounts(t, f.db, f.login.AccountID)
	if before["album_share_claims"] != 1 {
		t.Fatal("share claim missing")
	}
	used, reserved := photoJournalBalance(t, f.db, f.login.AccountID)
	if _, err = f.db.Exec(`ALTER TABLE album_account_object_purges ADD CONSTRAINT fail_media_settlement CHECK(media_settled_at IS NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.settleAccountMediaPurge(ctx, f.login.AccountID, d.RequestID, s, func() time.Time { return *f.now }); err == nil {
		t.Fatal("failed final receipt accepted")
	}
	for table, n := range mediaSettlementCounts(t, f.db, f.login.AccountID) {
		if n != before[table] {
			t.Error("partial erasure committed", table)
		}
	}
	a, b := photoJournalBalance(t, f.db, f.login.AccountID)
	if a != used || b != reserved {
		t.Fatal("quota escaped rollback")
	}
	if _, err = f.db.Exec(`ALTER TABLE album_account_object_purges DROP CHECK fail_media_settlement`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan time.Time, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at, e := NewMySQLUploads(f.db).settleAccountMediaPurge(ctx, f.login.AccountID, d.RequestID, s, func() time.Time { return *f.now })
			if e != nil {
				t.Error(e)
			}
			results <- at
		}()
	}
	wg.Wait()
	close(results)
	for at := range results {
		if !at.Equal(*f.now) {
			t.Fatal("replay changed receipt", at)
		}
	}
	for table, n := range mediaSettlementCounts(t, f.db, f.login.AccountID) {
		if n != 0 {
			t.Error("retained media", table)
		}
	}
	if _, err = f.service.Lookup(ctx, token); err == nil {
		t.Fatal("erased share still resolves")
	}
	var billing, sessions, grants int
	for table, count := range map[string]*int{"album_cloud_subscriptions": &billing, "album_sessions": &sessions, "album_apple_grants": &grants} {
		column := "account_id"
		if table == "album_cloud_subscriptions" {
			column = "owner_id"
		}
		if err = f.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+column+`=?`, f.login.AccountID).Scan(count); err != nil {
			t.Fatal(err)
		}
	}
	if billing != 1 || sessions != 1 || grants != 1 {
		t.Fatal("other erasure stages lost their records", billing, sessions, grants)
	}
}

type settlementInventoryFault struct {
	*accountPurgeTestStorage
	fault func() error
	calls int
}

func (s *settlementInventoryFault) listAccountPurgeVersions(ctx context.Context, owner, area string) ([]accountPurgeVersion, error) {
	s.calls++
	if s.fault != nil {
		f := s.fault
		s.fault = nil
		if err := f(); err != nil {
			return nil, err
		}
	}
	return s.accountPurgeTestStorage.listAccountPurgeVersions(ctx, owner, area)
}

func TestMySQLAccountMediaSettlementReopensNonemptyAreaWithoutErasingIndex(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	s.finish(t, s)
	ctx := context.Background()
	s.versions["originals"] = []accountPurgeVersion{s.version("originals", "unremoved", false)}
	if _, err := f.repo.settleAccountMediaPurge(ctx, f.login.AccountID, s.request, s, func() time.Time { return *f.now }); !errors.Is(err, ErrDerivedPending) {
		t.Fatal(err)
	}
	p, err := f.repo.accountObjectPurge(ctx, f.login.AccountID)
	if err != nil || p.State != "queued" || p.Area != 1 {
		t.Fatal("area not reopened", p, err)
	}
	if mediaSettlementCounts(t, f.db, f.login.AccountID)["album_asset_versions"] != 1 {
		t.Fatal("index erased before objects")
	}
	s.finish(t, s)
	if _, err = f.repo.settleAccountMediaPurge(ctx, f.login.AccountID, s.request, s, func() time.Time { return *f.now }); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLAccountMediaSettlementRejectsInvalidOrStaleEvidence(t *testing.T) {
	for _, mode := range []string{"unfinished", "wrong-request", "provider-error", "cancel", "fence-changed", "missing-schema"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newAccountPurgeFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := s.request
			if mode != "unfinished" {
				s.finish(t, s)
			}
			provider := &settlementInventoryFault{accountPurgeTestStorage: s}
			switch mode {
			case "wrong-request":
				request = uuid.NewString()
			case "provider-error":
				provider.fault = func() error { return ErrCOSOperation }
			case "cancel":
				provider.fault = func() error { cancel(); return context.Canceled }
			case "fence-changed":
				provider.fault = func() error {
					_, e := f.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=? WHERE owner_id=?`, f.now.Add(time.Second), f.login.AccountID)
					return e
				}
			case "missing-schema":
				if _, err := f.db.Exec(`ALTER TABLE album_account_object_purges DROP CHECK album_account_media_settled`); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`ALTER TABLE album_account_object_purges RENAME COLUMN media_settled_at TO missing_media_settled_at`); err != nil {
					t.Fatal(err)
				}
				if err := f.repo.CheckLibrarySchema(ctx); err == nil {
					t.Fatal("missing migration accepted")
				}
			}
			if _, err := f.repo.settleAccountMediaPurge(ctx, f.login.AccountID, request, provider, func() time.Time { return *f.now }); err == nil {
				t.Fatal("unsafe settlement accepted")
			}
			if mediaSettlementCounts(t, f.db, f.login.AccountID)["album_asset_versions"] != 1 {
				t.Fatal("failed settlement erased metadata")
			}
			if (mode == "unfinished" || mode == "wrong-request" || mode == "missing-schema") && provider.calls != 0 {
				t.Fatal("provider queried before admission")
			}
		})
	}
}

func TestMySQLAccountMediaSettlementNanosecondClock(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	*f.now = f.now.Add(999 * time.Nanosecond)
	s.finish(t, s)
	at, err := f.repo.settleAccountMediaPurge(context.Background(), f.login.AccountID, s.request, s, func() time.Time { return *f.now })
	if err != nil || !at.Equal(f.now.Truncate(time.Microsecond)) {
		t.Fatal("database rounded removal beyond settlement clock", at, err)
	}
}
