package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func readyRecordErasure(t *testing.T, x accessErasureFixture) *MySQLUploads {
	t.Helper()
	if _, err := x.f.accounts.eraseAccountAccess(context.Background(), x.d.AccountID, x.d.RequestID, x.f.now); err != nil {
		t.Fatal(err)
	}
	return NewMySQLUploads(x.f.r.db).WithSubscriptionEnvironment("Sandbox")
}

func TestMySQLAccountRecordErasureReplayRecreationAndIsolation(t *testing.T) {
	x := newAccessErasureFixture(t, true, true)
	r := readyRecordErasure(t, x)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at, err := r.eraseAccountRecords(ctx, x.d.AccountID, x.d.RequestID, func() time.Time { return x.f.now })
			if err != nil || !at.Equal(x.f.now) {
				t.Error("concurrent erasure", at, err)
			}
		}()
	}
	wg.Wait()
	for _, query := range []string{`SELECT COUNT(*) FROM album_accounts WHERE account_id=?`, `SELECT COUNT(*) FROM album_storage_accounts WHERE owner_id=?`, `SELECT COUNT(*) FROM album_account_deletions WHERE account_id=?`, `SELECT COUNT(*) FROM album_account_object_purges WHERE account_id=?`} {
		var count int
		if err := x.f.r.db.QueryRow(query, x.d.AccountID).Scan(&count); err != nil || count != 0 {
			t.Fatal("online account records retained", count, err)
		}
	}
	if !deletionAuthenticated(x.f, x.other.Token) {
		t.Fatal("unrelated account lost access")
	}
	if receipt, err := x.f.accounts.requestAccountDeletion(ctx, x.first.Token, x.d.RequestID, func() time.Time { return x.f.now.Add(time.Hour) }); err != nil || receipt != x.d {
		t.Fatal("accepted deletion replay lost", receipt, err)
	}
	if receipt, err := x.f.accounts.withdrawAccountDeletion(ctx, x.first.Token, x.d.RequestID, func() time.Time { return x.f.now }); err != nil || receipt == nil || *receipt != x.d {
		t.Fatal("accepted erasure mistaken for withdrawn request", receipt, err)
	}
	if _, err := r.eraseAccountRecords(ctx, x.d.AccountID, uuid.NewString(), func() time.Time { return x.f.now }); err == nil {
		t.Fatal("wrong request replayed")
	}
	if _, err := x.f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Subject: "erasure-owner", IssuedAt: x.f.now}, RefreshToken: uuid.NewString()}); !errors.Is(err, ErrAppleCode) {
		t.Fatal("in-flight pre-erasure authorization recreated account", err)
	}
	rebuilt, err := x.f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Subject: "erasure-owner", IssuedAt: x.f.now.Add(2 * time.Second)}, RefreshToken: uuid.NewString()})
	if err != nil || rebuilt.AccountID == x.d.AccountID {
		t.Fatal("fresh authorization did not create a new media account", rebuilt, err)
	}
	if at, err := r.eraseAccountRecords(ctx, x.d.AccountID, x.d.RequestID, func() time.Time { return x.f.now.Add(time.Minute) }); err != nil || !at.Equal(x.f.now) {
		t.Fatal("late erasure retry changed completion", at, err)
	}
	if _, err := r.CloudAccount(ctx, rebuilt.AccountID, x.f.now); err != nil {
		t.Fatal("old erasure retry damaged recreated account", err)
	}
}

func TestMySQLAccountRecordErasureRequiresReceiptsAndRollsBack(t *testing.T) {
	for _, mode := range []string{"access", "media", "ledger", "wrong-request", "remaining-grant", "receipt-failure"} {
		t.Run(mode, func(t *testing.T) {
			x := newAccessErasureFixture(t, true, true)
			r := readyRecordErasure(t, x)
			query := map[string]string{
				"access":          `UPDATE album_account_deletions SET access_erased_at=NULL WHERE account_id=?`,
				"media":           `UPDATE album_account_object_purges SET media_settled_at=NULL WHERE account_id=?`,
				"ledger":          `DELETE FROM album_deletion_ledger WHERE account_id=?`,
				"remaining-grant": `INSERT INTO album_apple_grants(grant_id,account_id,token_sha256,encrypted_token,token_nonce) VALUES (UUID(),?,UNHEX(REPEAT('aa',32)),X'01',UNHEX(REPEAT('00',12)))`,
			}[mode]
			if query != "" {
				if _, err := x.f.r.db.Exec(query, x.d.AccountID); err != nil {
					t.Fatal(err)
				}
			}
			request := x.d.RequestID
			if mode == "wrong-request" {
				request = uuid.NewString()
			}
			if mode == "receipt-failure" {
				if _, err := x.f.r.db.Exec(`ALTER TABLE album_deletion_ledger ADD CONSTRAINT fail_erasure_receipt CHECK(records_erased_at IS NULL)`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.eraseAccountRecords(context.Background(), x.d.AccountID, request, func() time.Time { return x.f.now }); err == nil {
				t.Fatal("incomplete erasure accepted")
			}
			for _, query := range []string{`SELECT COUNT(*) FROM album_accounts WHERE account_id=?`, `SELECT COUNT(*) FROM album_storage_accounts WHERE owner_id=?`, `SELECT COUNT(*) FROM album_account_deletions WHERE account_id=?`, `SELECT COUNT(*) FROM album_account_object_purges WHERE account_id=?`} {
				var count int
				if err := x.f.r.db.QueryRow(query, x.d.AccountID).Scan(&count); err != nil || count != 1 {
					t.Fatal("failed erasure partially removed account", count, err)
				}
			}
			if mode == "receipt-failure" {
				if _, err := x.f.r.db.Exec(`ALTER TABLE album_deletion_ledger DROP CHECK fail_erasure_receipt`); err != nil {
					t.Fatal(err)
				}
				if _, err := r.eraseAccountRecords(context.Background(), x.d.AccountID, request, func() time.Time { return x.f.now }); err != nil {
					t.Fatal("retry after failed receipt", err)
				}
			}
		})
	}
}

func TestMySQLAccountRecordErasureRechecksStaleSubscriptionsAndRestoresPurchase(t *testing.T) {
	f, r, s, provider, deletion := completionFixture(t)
	ctx := context.Background()
	if b, err := s.runSubscriptionBindingBatch(ctx, r, 10); err != nil || b.Acknowledged != 1 {
		t.Fatal(b, err)
	}
	media := sharingFixture{db: f.r.db, repo: r, login: AccountLogin{AccountID: deletion.AccountID}, now: &f.now}
	storage := readyAccountMediaSettlement(t, media, deletion.RequestID)
	if _, err := r.settleAccountMediaPurge(ctx, deletion.AccountID, deletion.RequestID, storage, func() time.Time { return f.now }); err != nil {
		t.Fatal(err)
	}
	if b, err := f.accounts.runAppleDeletionBatch(ctx, func(context.Context, string) error { return nil }, 10, func() time.Time { return f.now }); err != nil || b.Acknowledged != 1 {
		t.Fatal(b, err)
	}
	if _, err := f.accounts.eraseAccountAccess(ctx, deletion.AccountID, deletion.RequestID, f.now); err != nil {
		t.Fatal(err)
	}
	clockReads := 0
	if _, err := r.eraseAccountRecords(ctx, deletion.AccountID, deletion.RequestID, func() time.Time {
		clockReads++
		if clockReads == 2 {
			f.now = f.now.Add(time.Minute)
		}
		return f.now
	}); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("stale receipt authorized record removal", err)
	}
	if revision, checked, _ := readCompletionReceipt(t, f, deletion.AccountID); revision.Valid || checked.Valid {
		t.Fatal("stale receipt not invalidated")
	}
	if count, err := r.runAccountRecordErasureBatch(ctx, 1, func() time.Time { return f.now }); err != nil || count != 0 {
		t.Fatal("pending subscriptions bypassed", count, err)
	}
	provider.payload["signedDate"] = f.now.UnixMilli()
	if b, err := s.runSubscriptionBindingBatch(ctx, r, 10); err != nil || b.Acknowledged != 2 {
		t.Fatal("could not recheck both chains", b, err)
	}
	if count, err := r.runAccountRecordErasureBatch(ctx, 10, func() time.Time { return f.now }); err != nil || count != 1 {
		t.Fatal("freshly verified record erasure", count, err)
	}
	for _, query := range []string{`SELECT COUNT(*) FROM album_cloud_subscriptions WHERE owner_id=?`, `SELECT COUNT(*) FROM album_subscription_notifications WHERE owner_id=?`, `SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=?`} {
		var count int
		if err := f.r.db.QueryRow(query, deletion.AccountID).Scan(&count); err != nil || count != 0 {
			t.Fatal("subscription data retained", count, err)
		}
	}
	f.now = f.now.Add(2 * time.Second)
	rebuilt, err := f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Subject: "binding-owner", IssuedAt: f.now}, RefreshToken: uuid.NewString()})
	if err != nil || rebuilt.AccountID == deletion.AccountID {
		t.Fatal(rebuilt, err)
	}
	provider.payload["signedDate"] = f.now.UnixMilli()
	restored, err := s.Sync(ctx, rebuilt.AccountID, provider.fixture.sign(t, provider.payload))
	if err != nil || restored.State != "active" || restored.UsedBytes != 0 || restored.ReservedBytes != 0 || restored.AppAccountToken != provider.target {
		t.Fatal("stable purchase did not restore to empty new account", restored, err)
	}
	if _, err := r.SubscriptionOwner(ctx, deletion.AccountID); !errors.Is(err, ErrSubscriptionBinding) {
		t.Fatal("old media account alias revived", err)
	}
}

func TestGatewayAccountRecordErasureRequiresExplicitEnablement(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			f := newAppleGatewayFixture(t)
			session, grant, _ := f.login(t, "record-worker")
			deletion, err := f.accounts.requestAccountDeletion(context.Background(), session.Token, uuid.NewString(), time.Now)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			media := sharingFixture{db: f.db, repo: NewMySQLUploads(f.db), login: grant, now: &now}
			storage := readyAccountMediaSettlement(t, media, deletion.RequestID)
			if _, err = media.repo.settleAccountMediaPurge(context.Background(), deletion.AccountID, deletion.RequestID, storage, func() time.Time { return now }); err != nil {
				t.Fatal(err)
			}
			fixture := newAlbumReceiptFixture(t, now)
			orphan := appleSubjectHash("retirement-worker-orphan")
			if _, err = f.db.Exec(`INSERT INTO album_apple_identity_state(subject_sha256,revoked_before) VALUES (?,?)`, orphan, now.Add(-2*identityFenceRetention)); err != nil {
				t.Fatal(err)
			}
			f.config.Subscriptions = &AlbumSubscriptionConfig{RebindingEnabled: true, RecordErasureEnabled: enabled, Environment: "Sandbox", IssuerID: uuid.NewString(), KeyID: "ALBUMSKEY1", SigningKey: albumTestECKey(t), Roots: fixture.roots}
			g, err := NewGateway(context.Background(), f.config, f.transport(t, func(*http.Request) (*http.Response, error) { return appleResponse(""), nil }))
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			deadline := time.Now().Add(12 * time.Second)
			for {
				var erased sql.NullTime
				var remaining int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_apple_identity_state WHERE subject_sha256=?`, orphan).Scan(&remaining); err != nil {
					t.Fatal(err)
				}
				if err := f.db.QueryRow(`SELECT records_erased_at FROM album_deletion_ledger WHERE account_id=?`, deletion.AccountID).Scan(&erased); err != nil {
					t.Fatal(err)
				}
				if enabled && erased.Valid && remaining == 0 {
					break
				}
				if !enabled {
					var access sql.NullTime
					if err := f.db.QueryRow(`SELECT access_erased_at FROM album_account_deletions WHERE account_id=?`, deletion.AccountID).Scan(&access); err != nil || erased.Valid || remaining != 1 {
						t.Fatal("disabled worker erased account", err)
					}
					if access.Valid {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("runtime erasure did not reach expected phase")
				}
				time.Sleep(25 * time.Millisecond)
			}
		})
	}
}

func TestGatewayAccountRecordErasureMissingSchemaFailsStartup(t *testing.T) {
	f := newAppleGatewayFixture(t)
	if _, err := f.db.Exec(`ALTER TABLE album_deletion_ledger DROP CHECK album_records_erased_after_request, RENAME COLUMN records_erased_at TO hidden_records_erased_at`); err != nil {
		t.Fatal(err)
	}
	g, err := NewGateway(context.Background(), f.config, nil)
	if g != nil {
		g.Close()
	}
	if err == nil {
		t.Fatal("missing online erasure receipt schema accepted")
	}
}
