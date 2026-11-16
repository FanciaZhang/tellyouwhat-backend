//go:build albums_live

package albums

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

// Read the real account inventory, but refuse every mutation outside the frozen
// versions and upload IDs of the probe's freshly created synthetic keys.
type liveAccountErasureStorage struct{ *liveTrashStorage }

func (s liveAccountErasureStorage) listAccountPurgeVersions(ctx context.Context, owner, area string) ([]accountPurgeVersion, error) {
	if owner != s.owner {
		return nil, ErrInvalidManifest
	}
	versions, err := s.cleaner.listAccountPurgeVersions(ctx, owner, area)
	for _, version := range versions {
		if !s.allowed[version.Target.Key][version.Version.VersionID] {
			return nil, ErrConflict
		}
	}
	return versions, err
}

func (s liveAccountErasureStorage) listAccountPurgeUploads(ctx context.Context, owner, area string) ([]accountPurgeUpload, error) {
	if owner != s.owner {
		return nil, ErrInvalidManifest
	}
	uploads, err := s.cleaner.listAccountPurgeUploads(ctx, owner, area)
	for _, upload := range uploads {
		if !s.uploads[upload.Target.Key][upload.UploadID] {
			return nil, ErrConflict
		}
	}
	return uploads, err
}

func TestLiveCOSAccountErasureFromHTTPToOnlineReceipt(t *testing.T) {
	if os.Getenv("ALBUM_ACCOUNT_ERASURE_COS_PROBE") != "1" {
		t.Skip("explicit owned synthetic account erasure probe required")
	}
	admin, err := mysql.ParseDSN(os.Getenv("MYSQL_TEST_ADMIN_DSN"))
	if err != nil || admin.Net != "unix" || !strings.HasSuffix(admin.DBName, "_test") {
		t.Fatal("isolated socket-only MySQL required")
	}
	info, err := os.Lstat(filepath.Dir(admin.Addr))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private MySQL socket directory required")
	}
	reader, root, ctx := liveRoundtripFixture(t)
	completionProbeLease(t, root)
	cleaner := completionProbeCleaner(t)
	in := liveTrashLoad(t, root)
	frozen, markers := liveTrashInventory(t, ctx, reader, cleaner, in)
	storage := liveAccountErasureStorage{frozen}
	for _, version := range in.Versions {
		if !frozen.allowed[version.Key][version.VersionID] {
			t.Fatal("prepared versions were partially consumed; retain intent")
		}
	}
	if !frozen.uploads[in.Keys[1]][in.Multipart] {
		t.Fatal("prepared multipart is no longer available; retain intent")
	}
	for _, area := range accountPurgeAreas {
		if _, err := storage.listAccountPurgeVersions(ctx, in.Owner, area); err != nil {
			t.Fatal("owned account version inventory unavailable", area, err)
		}
		if _, err := storage.listAccountPurgeUploads(ctx, in.Owner, area); err != nil {
			t.Fatal("owned account multipart inventory unavailable", area, err)
		}
	}

	f := newPushFixture(t)
	db := f.r.db
	f.now = seedLiveTrashDatabase(t, ctx, db, in)
	now := func() time.Time { return f.now }
	subject := "owned-cos-erasure-" + in.Owner
	if _, err := db.Exec(`UPDATE album_accounts SET apple_subject_sha256=? WHERE account_id=?`, appleSubjectHash(albumDevelopmentAudience, subject), in.Owner); err != nil {
		t.Fatal("synthetic account setup failed", err)
	}
	first, _ := f.login(t, subject, f.now)
	second, _ := f.login(t, subject, f.now)
	other, _ := f.login(t, "retained-"+in.Owner, f.now)
	if first.AccountID != in.Owner || second.AccountID != in.Owner || other.AccountID == in.Owner {
		t.Fatal("HTTP sessions do not belong to the owned account")
	}
	request := uuid.NewString()
	body := `{"requestID":"` + request + `"}`
	router := deletionRouter(t, f, &loginHTTPLimiter{}, true)
	accepted := sendDeletion(router, http.MethodPost, deletionPath, first.Token, body, nil)
	if accepted.Code != http.StatusAccepted || !strings.Contains(accepted.Body.String(), `"state":"accepted"`) ||
		deletionAuthenticated(f, first.Token) || deletionAuthenticated(f, second.Token) || !deletionAuthenticated(f, other.Token) {
		t.Fatal("deletion acceptance did not fence the owned sessions", accepted.Code)
	}
	r := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", now)
	if count, err := r.runAccountRecordErasureBatch(ctx, 10, now); err != nil || count != 0 {
		t.Fatal("account records became eligible before media and access cleanup", count, err)
	}
	if ready, waiting, err := r.checkAccountMediaQuiescence(ctx, in.Owner, request, now()); err != nil || !ready || waiting {
		t.Fatal("synthetic writers did not close", ready, waiting, err)
	}

	// Hide one successful real COS response, then retry the same immutable version.
	frozen.lose = true
	worker := &AccountMediaErasureWorker{repository: r, storage: storage, batchSize: 1, now: now}
	settled, lostObserved := 0, false
	for pass := 0; pass < 20 && settled == 0; pass++ {
		batch, err := worker.RunOnce(ctx)
		if err != nil {
			t.Fatal("owned account media erasure failed; retain intent", err)
		}
		if batch.Deferred > 0 {
			if lostObserved || frozen.lostVersion == "" {
				t.Fatal("unexpected media deferral; retain intent", batch)
			}
			used, _ := photoJournalBalance(t, db, in.Owner)
			if used != 4096 {
				t.Fatal("unknown deletion response released capacity")
			}
			lostObserved = true
			f.now = f.now.Add(11 * time.Minute)
			r = NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", now)
			worker.repository = r
		} else {
			f.now = f.now.Add(6 * time.Second)
		}
		settled += batch.Settled
	}
	lostAttempts := 0
	for _, attempt := range frozen.deleteAttempts {
		if attempt.Key == frozen.lostKey && attempt.VersionID == frozen.lostVersion {
			lostAttempts++
		}
	}
	if settled != 1 || !lostObserved || lostAttempts != 2 || frozen.aborts != 1 {
		t.Fatal("owned media was not settled after the exact retry", settled, lostAttempts, frozen.aborts)
	}
	for _, area := range accountPurgeAreas {
		empty, err := accountPrefixEmpty(ctx, storage, in.Owner, area)
		if err != nil || !empty {
			t.Fatal("final account inventory is not empty", area, err)
		}
	}
	for table, count := range mediaSettlementCounts(t, db, in.Owner) {
		if count != 0 {
			t.Fatal("account media metadata remains", table, count)
		}
	}
	if used, reserved := photoJournalBalance(t, db, in.Owner); used != 0 || reserved != 0 {
		t.Fatal("account capacity was not settled", used, reserved)
	}
	if _, err := f.accounts.eraseAccountAccess(ctx, in.Owner, request, now()); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("access erasure bypassed Apple revocation acknowledgement", err)
	}
	if batch, err := f.accounts.runAppleDeletionBatch(ctx, func(context.Context, string) error { return nil }, 10, now); err != nil || batch.Acknowledged != 2 {
		t.Fatal("controlled Apple revocations did not finish", batch, err)
	}
	if count, err := f.accounts.runAccessErasureBatch(ctx, 10, now); err != nil || count != 1 {
		t.Fatal("access records were not erased", count, err)
	}
	if count, err := r.runAccountRecordErasureBatch(ctx, 10, now); err != nil || count != 1 {
		t.Fatal("online account records were not erased", count, err)
	}
	if at, err := r.erasedAccountRecords(ctx, in.Owner, request); err != nil || !at.Equal(now()) {
		t.Fatal("online erasure receipt invalid", err)
	}
	for table, count := range accessRecordCounts(t, db, in.Owner) {
		if count != 0 {
			t.Fatal("access records remain after the online receipt", table, count)
		}
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM album_accounts WHERE account_id=?`,
		`SELECT COUNT(*) FROM album_storage_accounts WHERE owner_id=?`,
		`SELECT COUNT(*) FROM album_account_deletions WHERE account_id=?`,
		`SELECT COUNT(*) FROM album_account_object_purges WHERE account_id=?`,
	} {
		var count int
		if err := db.QueryRow(query, in.Owner).Scan(&count); err != nil || count != 0 {
			t.Fatal("online records remain after the receipt", count, err)
		}
	}
	if reply := sendDeletion(deletionRouter(t, f, &loginHTTPLimiter{}, true), http.MethodPost, deletionPath, first.Token, body, nil); reply.Code != accepted.Code || reply.Body.String() != accepted.Body.String() || !deletionAuthenticated(f, other.Token) {
		t.Fatal("deletion replay or unrelated account access changed", reply.Code)
	}
	photoMultipartProbeCheckpoint(t, root, "account-erasure-result.json", map[string]any{
		"realCOS": true, "realMySQL": true, "syntheticAccount": true, "httpAccepted": true,
		"emptyPrefixes": len(accountPurgeAreas), "deleteMarkersObserved": markers, "multipartAborts": frozen.aborts,
		"lostReplyVersionAttempts": lostAttempts, "exactDeleteAttempts": frozen.deleteAttempts,
		"onlineErasureReceipt": true, "onlineRecordsReadBackAbsent": true, "unrelatedAccountPreserved": true, "controlledAppleRevocation": true,
		"realAppleOrSubscriptionAcceptance": false, "historicalBackupErasure": false,
	})
}
