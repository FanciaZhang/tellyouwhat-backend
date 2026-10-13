package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type accessErasureFixture struct {
	f                    *pushFixture
	d                    accountDeletionRequest
	first, second, other AlbumSession
	grants               []AccountLogin
}

func newAccessErasureFixture(t *testing.T, media, acknowledged bool) accessErasureFixture {
	t.Helper()
	f := newPushFixture(t)
	a, g := f.login(t, "erasure-owner", f.now)
	b, g2 := f.login(t, "erasure-owner", f.now)
	other, keep := f.login(t, "retained-owner", f.now)
	for i, s := range []AlbumSession{a, b, other} {
		p, err := f.r.update(context.Background(), s.Token, pushUpdate(strings.Repeat([]string{"ab", "cd", "ef"}[i], 32), 0), f.now)
		if err != nil {
			t.Fatal(err)
		}
		reminder := uuid.NewString()
		if _, err = f.r.db.Exec(`INSERT INTO album_retention_reminders(reminder_id,owner_id,environment,retention_deadline,days_before,due_at,expires_at,available_at,state) VALUES (?,?,'Sandbox',?,30,?,?,?,'pending')`, reminder, s.AccountID, f.now.Add(30*24*time.Hour+time.Duration(i)*time.Second), f.now, f.now.Add(time.Hour), f.now); err != nil {
			t.Fatal(err)
		}
		if _, err = f.r.db.Exec(`INSERT INTO album_retention_deliveries(delivery_id,reminder_id,registration_id,registration_revision,available_at,lease_token) VALUES (?,?,?,?,?,?)`, uuid.NewString(), reminder, p.ID, p.Revision, f.now, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	d := acceptAppleDeletion(t, f, a.Token)
	x := accessErasureFixture{f, d, a, b, other, []AccountLogin{g, g2, keep}}
	if media {
		x.finishMedia(t)
	}
	if acknowledged {
		x.revoke(t)
	}
	return x
}

func (x accessErasureFixture) finishMedia(t *testing.T) {
	t.Helper()
	f := sharingFixture{db: x.f.r.db, repo: NewMySQLUploads(x.f.r.db), login: x.grants[0], now: &x.f.now}
	s := readyAccountMediaSettlement(t, f, x.d.RequestID)
	if _, err := f.repo.settleAccountMediaPurge(context.Background(), x.d.AccountID, x.d.RequestID, s, func() time.Time { return x.f.now }); err != nil {
		t.Fatal(err)
	}
}
func (x accessErasureFixture) revoke(t *testing.T) {
	t.Helper()
	b, err := x.f.accounts.runAppleDeletionBatch(context.Background(), func(context.Context, string) error { return nil }, 10, func() time.Time { return x.f.now })
	if err != nil || b.Acknowledged != 2 {
		t.Fatal(b, err)
	}
}
func accessRecordCounts(t *testing.T, db *sql.DB, owner string) map[string]int {
	t.Helper()
	queries := map[string]string{
		"sessions":   `SELECT COUNT(*) FROM album_sessions WHERE account_id=?`,
		"grants":     `SELECT COUNT(*) FROM album_apple_grants WHERE account_id=?`,
		"apple-jobs": `SELECT COUNT(*) FROM album_deletion_apple_jobs WHERE account_id=?`,
		"reminders":  `SELECT COUNT(*) FROM album_retention_reminders WHERE owner_id=?`,
		"push":       `SELECT COUNT(*) FROM album_push_registrations p JOIN album_sessions s ON s.session_id=p.session_id WHERE s.account_id=?`,
		"deliveries": `SELECT COUNT(*) FROM album_retention_deliveries d JOIN album_retention_reminders m ON m.reminder_id=d.reminder_id WHERE m.owner_id=?`,
	}
	out := map[string]int{}
	for name, query := range queries {
		var n int
		if err := db.QueryRow(query, owner).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[name] = n
	}
	return out
}

func TestMySQLAccountAccessErasureRemovesCredentialsAndPreservesExactRequestReplay(t *testing.T) {
	x := newAccessErasureFixture(t, true, true)
	ctx := context.Background()
	before := accessRecordCounts(t, x.f.r.db, x.other.AccountID)
	at, err := x.f.accounts.eraseAccountAccess(ctx, x.d.AccountID, x.d.RequestID, x.f.now)
	if err != nil || !at.Equal(x.f.now) {
		t.Fatal(at, err)
	}
	for name, n := range accessRecordCounts(t, x.f.r.db, x.d.AccountID) {
		if n != 0 {
			t.Error("retained access data", name, n)
		}
	}
	for name, n := range accessRecordCounts(t, x.f.r.db, x.other.AccountID) {
		if n != before[name] {
			t.Error("foreign account changed", name)
		}
	}
	if !deletionAuthenticated(x.f, x.other.Token) || deletionAuthenticated(x.f, x.first.Token) {
		t.Fatal("incorrect authentication boundary")
	}
	if _, err = x.f.accounts.RefreshCredential(ctx, x.other.AccountID, x.grants[2].GrantID); err != nil {
		t.Fatal("other provider credential removed", err)
	}
	for range 2 {
		reopened, _ := NewMySQLAccounts(x.f.r.db, x.f.r.cipher)
		d, e := reopened.requestAccountDeletion(ctx, x.first.Token, x.d.RequestID, func() time.Time { return x.f.now.AddDate(1, 0, 0) })
		if e != nil || d != x.d {
			t.Fatal("lost-response replay failed after session erasure", e)
		}
	}
	for _, input := range []struct{ token, id string }{{x.second.Token, x.d.RequestID}, {x.other.Token, x.d.RequestID}, {x.first.Token, uuid.NewString()}} {
		if _, err = x.f.accounts.requestAccountDeletion(ctx, input.token, input.id, func() time.Time { return x.f.now }); err == nil {
			t.Fatal("unrelated credential/request replay accepted")
		}
	}
	if _, err = x.f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: freshAppleIdentity("erasure-owner"), RefreshToken: "another-grant"}); !errors.Is(err, ErrOwner) {
		t.Fatal("login reopened unfinished account deletion", err)
	}
	if _, err = x.f.sessions.Issue(ctx, x.grants[0]); !errors.Is(err, ErrSession) {
		t.Fatal("deleted grant issued a session", err)
	}
	var state string
	if err = x.f.r.db.QueryRow(`SELECT state FROM album_accounts WHERE account_id=?`, x.d.AccountID).Scan(&state); err != nil || state != "deleting" {
		t.Fatal("full erasure implied", err)
	}
}

func TestMySQLAccountAccessErasureNeedsEveryAcknowledgementAndMediaSettlement(t *testing.T) {
	for _, mode := range []string{"pending-apple", "missing-job", "unwiped-token", "unwiped-nonce", "pending-media", "wrong-request", "wrong-fence"} {
		t.Run(mode, func(t *testing.T) {
			x := newAccessErasureFixture(t, mode != "pending-media", mode != "pending-apple")
			request := x.d.RequestID
			switch mode {
			case "missing-job":
				if _, err := x.f.r.db.Exec(`DELETE FROM album_deletion_apple_jobs WHERE grant_id=?`, x.grants[0].GrantID); err != nil {
					t.Fatal(err)
				}
			case "unwiped-token":
				if _, err := x.f.r.db.Exec(`UPDATE album_apple_grants SET encrypted_token=X'01' WHERE grant_id=?`, x.grants[0].GrantID); err != nil {
					t.Fatal(err)
				}
			case "unwiped-nonce":
				if _, err := x.f.r.db.Exec(`UPDATE album_apple_grants SET token_nonce=UNHEX(REPEAT('aa',12)) WHERE grant_id=?`, x.grants[0].GrantID); err != nil {
					t.Fatal(err)
				}
			case "wrong-request":
				request = uuid.NewString()
			case "wrong-fence":
				if _, err := x.f.r.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=NULL WHERE owner_id=?`, x.d.AccountID); err != nil {
					t.Fatal(err)
				}
			}
			before := accessRecordCounts(t, x.f.r.db, x.d.AccountID)
			if _, err := x.f.accounts.eraseAccountAccess(context.Background(), x.d.AccountID, request, x.f.now); err == nil {
				t.Fatal("incomplete cleanup evidence accepted")
			}
			for name, n := range accessRecordCounts(t, x.f.r.db, x.d.AccountID) {
				if n != before[name] {
					t.Error("failed erasure removed records", name)
				}
			}
			if mode == "pending-apple" || mode == "missing-job" || mode == "unwiped-token" || mode == "unwiped-nonce" || mode == "pending-media" {
				if n, err := x.f.accounts.runAccessErasureBatch(context.Background(), 10, func() time.Time { return x.f.now }); err != nil || n != 0 {
					t.Fatal("ineligible queue candidate", n, err)
				}
			}
		})
	}
}

func TestMySQLAccountAccessErasureAtomicRollbackAndConcurrentRetry(t *testing.T) {
	x := newAccessErasureFixture(t, true, true)
	ctx := context.Background()
	before := accessRecordCounts(t, x.f.r.db, x.d.AccountID)
	if _, err := x.f.r.db.Exec(`ALTER TABLE album_account_deletions ADD CONSTRAINT fail_access_receipt CHECK(access_erased_at IS NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.accounts.eraseAccountAccess(ctx, x.d.AccountID, x.d.RequestID, x.f.now); err == nil {
		t.Fatal("failed receipt accepted")
	}
	for name, n := range accessRecordCounts(t, x.f.r.db, x.d.AccountID) {
		if n != before[name] {
			t.Error("partial credential erasure", name)
		}
	}
	if _, err := x.f.r.db.Exec(`ALTER TABLE album_account_deletions DROP CHECK fail_access_receipt`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo, _ := NewMySQLAccounts(x.f.r.db, x.f.r.cipher)
			at, e := repo.eraseAccountAccess(ctx, x.d.AccountID, x.d.RequestID, x.f.now)
			if e != nil || !at.Equal(x.f.now) {
				t.Error(at, e)
			}
		}()
	}
	wg.Wait()
	if n, err := x.f.accounts.runAccessErasureBatch(ctx, 10, func() time.Time { return x.f.now.Add(time.Minute) }); err != nil || n != 0 {
		t.Fatal("erased records queued again", n, err)
	}
	if _, err := x.f.accounts.requestAccountDeletion(ctx, x.first.Token, x.d.RequestID, func() time.Time { return x.f.now }); err != nil {
		t.Fatal("retry after rollback and erasure failed", err)
	}
}

func TestGatewayAccountAccessErasureFollowsAppleAndMediaReceipts(t *testing.T) {
	f := newAppleGatewayFixture(t)
	session, grant, _ := f.login(t, "runtime-erasure")
	other, _, _ := f.login(t, "runtime-retained")
	d, err := f.accounts.requestAccountDeletion(context.Background(), session.Token, uuid.NewString(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	media := sharingFixture{db: f.db, repo: NewMySQLUploads(f.db), login: grant, now: &now}
	s := readyAccountMediaSettlement(t, media, d.RequestID)
	if _, err = media.repo.settleAccountMediaPurge(context.Background(), d.AccountID, d.RequestID, s, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	g, err := NewGateway(context.Background(), f.config, f.transport(t, func(*http.Request) (*http.Response, error) { calls.Add(1); return appleResponse(""), nil }))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	waitAppleGateway(t, func() bool {
		var at sql.NullTime
		e := f.db.QueryRow(`SELECT access_erased_at FROM album_account_deletions WHERE account_id=?`, session.AccountID).Scan(&at)
		return e == nil && at.Valid
	})
	if calls.Load() != 1 {
		t.Fatal("unexpected revocation calls", calls.Load())
	}
	for name, n := range accessRecordCounts(t, f.db, session.AccountID) {
		if n != 0 {
			t.Error("runtime retained access data", name)
		}
	}
	if _, err = f.accounts.requestAccountDeletion(context.Background(), session.Token, d.RequestID, time.Now); err != nil {
		t.Fatal("runtime cleanup broke replay", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://albums.example/v1/albums", nil)
	req.Header.Set("Authorization", "Bearer "+other.Token)
	if _, err = f.sessions.Authenticate(context.Background(), req); err != nil {
		t.Fatal("runtime affected unrelated session", err)
	}
}

func TestGatewayAccountAccessErasureMissingSchemaFailsStartup(t *testing.T) {
	f := newAppleGatewayFixture(t)
	if _, err := f.db.Exec(`ALTER TABLE album_account_deletions DROP CHECK album_access_erased_after_request`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`ALTER TABLE album_account_deletions RENAME COLUMN access_erased_at TO missing_access_erased_at`); err != nil {
		t.Fatal(err)
	}
	g, err := NewGateway(context.Background(), f.config, f.transport(t, func(*http.Request) (*http.Response, error) {
		t.Error("provider called before schema check")
		return nil, ErrAppleCode
	}))
	if g != nil {
		g.Close()
	}
	if err == nil {
		t.Fatal("missing migration accepted")
	}
}
