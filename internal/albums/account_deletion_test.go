package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func deletionAuthenticated(f *pushFixture, token string) bool {
	r := httptest.NewRequest("GET", "https://albums.example/v1/albums/account", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	_, err := f.sessions.Authenticate(context.Background(), r)
	return err == nil
}

func TestMySQLAccountDeletionRecordedAccessRevocationAndExactReplay(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	first, grant := f.login(t, "delete-owner", f.now)
	second, _ := f.login(t, "delete-owner", f.now.Add(time.Second))
	other, _ := f.login(t, "keep-owner", f.now)
	registered, err := f.r.update(ctx, first.Token, pushUpdate(strings.Repeat("ab", 32), 0), f.now)
	if err != nil {
		t.Fatal(err)
	}
	version, share := uuid.NewString(), uuid.NewString()
	if _, err := f.r.db.Exec(`INSERT INTO album_asset_versions(owner_id,asset_id,version_id,manifest_digest,manifest_json,sealed_objects,original_bytes,published_at) VALUES (?,'synthetic',?,?,'{}','[]',1,?)`, first.AccountID, version, strings.Repeat("a", 64), f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.db.Exec(`INSERT INTO album_share_links(share_id,owner_id,version_id,request_id,key_id,request_digest,token_digest,max_claims,created_at,expires_at) VALUES (?,?,?,?, 'test',UNHEX(REPEAT('aa',32)),UNHEX(REPEAT('bb',32)),1,?,?)`, share, first.AccountID, version, uuid.NewString(), f.now, f.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.db.Exec(`INSERT INTO album_retention_reminders(reminder_id,owner_id,environment,retention_deadline,days_before,due_at,expires_at,available_at,state) VALUES (?,?,'Sandbox',?,1,?,?,?,'pending')`, uuid.NewString(), first.AccountID, f.now.Add(24*time.Hour), f.now, f.now.Add(time.Hour), f.now); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	clock := func() time.Time { return f.now }
	got, err := f.accounts.requestAccountDeletion(ctx, first.Token, id, clock)
	if err != nil || got.AccountID != first.AccountID || got.RequestID != id || !got.RequestedAt.Equal(f.now) {
		t.Fatal("request not durably accepted", err)
	}
	if deletionAuthenticated(f, first.Token) || deletionAuthenticated(f, second.Token) || !deletionAuthenticated(f, other.Token) {
		t.Fatal("wrong account access cutover")
	}
	if _, err := f.r.target(ctx, first.AccountID, registered.ID, registered.Revision); err == nil {
		t.Fatal("revoked push still available")
	}
	var state string
	var revoked sql.NullTime
	var tokens, versions int
	if err := f.r.db.QueryRow(`SELECT revoked_at FROM album_share_links WHERE share_id=?`, share).Scan(&revoked); err != nil || !revoked.Valid {
		t.Fatal("share not revoked", err)
	}
	if err := f.r.db.QueryRow(`SELECT state FROM album_retention_reminders WHERE owner_id=?`, first.AccountID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatal("reminder not cancelled", err)
	}
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_push_registrations WHERE registration_id=? AND (encrypted_token IS NOT NULL OR token_nonce IS NOT NULL OR token_sha256 IS NOT NULL)`, registered.ID).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatal("push token retained", err)
	}
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_asset_versions WHERE owner_id=?`, first.AccountID).Scan(&versions); err != nil || versions != 1 {
		t.Fatal("request erased media records prematurely", err)
	}
	// Keep provider credentials for the separately verified Apple revoke job.
	if token, err := f.accounts.RefreshCredential(ctx, first.AccountID, grant.GrantID); err != nil || token == "" {
		t.Fatal("provider cleanup credential lost")
	}
	if _, err := f.sessions.Issue(ctx, grant); !errors.Is(err, ErrSession) {
		t.Fatal("new session issued during deletion", err)
	}
	if _, err := f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Subject: "delete-owner"}, RefreshToken: "new-provider-grant"}); !errors.Is(err, ErrOwner) {
		t.Fatal("login revived deleting account", err)
	}
	reopened, _ := NewMySQLAccounts(f.r.db, f.r.cipher)
	replayed, err := reopened.requestAccountDeletion(ctx, first.Token, id, func() time.Time { return f.now.Add(60 * 24 * time.Hour) })
	if err != nil || replayed != got {
		t.Fatal("lost-response retry changed receipt", err)
	}
	for _, item := range []struct{ token, id string }{{second.Token, id}, {first.Token, uuid.NewString()}, {other.Token, id}} {
		if _, err := reopened.requestAccountDeletion(ctx, item.token, item.id, clock); err == nil {
			t.Fatal("different credential/request replay accepted")
		}
	}
	if !deletionAuthenticated(f, other.Token) {
		t.Fatal("request collision disabled unrelated account")
	}
}

func TestMySQLAccountDeletionFailureRollsBackAndRetrySucceeds(t *testing.T) {
	f := newPushFixture(t)
	s, _ := f.login(t, "owner", f.now)
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := f.r.db.Exec(`ALTER TABLE album_share_links RENAME COLUMN revoked_at TO unavailable_revoked_at`); err != nil {
		t.Fatal(err)
	}
	_, err := f.accounts.requestAccountDeletion(ctx, s.Token, id, func() time.Time { return f.now })
	if err == nil {
		t.Fatal("incomplete transaction accepted")
	}
	if _, err := f.r.db.Exec(`ALTER TABLE album_share_links RENAME COLUMN unavailable_revoked_at TO revoked_at`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_account_deletions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial receipt survived", err)
	}
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_apple_jobs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed request left provider work", err)
	}
	if !deletionAuthenticated(f, s.Token) {
		t.Fatal("failed request revoked live account")
	}
	if _, err := f.accounts.requestAccountDeletion(ctx, s.Token, id, func() time.Time { return f.now }); err != nil {
		t.Fatal("retry failed", err)
	}
}

func TestMySQLAccountDeletionRejectsInvalidExpiredOrRevokedCredentials(t *testing.T) {
	for _, mode := range []string{"invalid", "expired", "session-revoked", "grant-revoked", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := newPushFixture(t)
			s, g := f.login(t, "owner", f.now)
			token := s.Token
			switch mode {
			case "invalid":
				token = "invalid"
			case "expired":
				_, _ = f.r.db.Exec(`UPDATE album_sessions SET expires_at=?`, f.now)
			case "session-revoked":
				if err := f.sessions.Revoke(context.Background(), token); err != nil {
					t.Fatal(err)
				}
			case "grant-revoked":
				_, _ = f.r.db.Exec(`UPDATE album_apple_grants SET revoked_at=? WHERE grant_id=?`, f.now, g.GrantID)
			case "disabled":
				_, _ = f.r.db.Exec(`UPDATE album_accounts SET state='disabled' WHERE account_id=?`, s.AccountID)
			}
			if _, err := f.accounts.requestAccountDeletion(context.Background(), token, uuid.NewString(), func() time.Time { return f.now }); !errors.Is(err, ErrSession) {
				t.Fatal("invalid credential accepted", err)
			}
			var count int
			if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_account_deletions`).Scan(&count); err != nil || count != 0 {
				t.Fatal("invalid request persisted", err)
			}
		})
	}
}

func TestMySQLAccountDeletionConcurrentExactRetriesConverge(t *testing.T) {
	f := newPushFixture(t)
	s, _ := f.login(t, "owner", f.now)
	id := uuid.NewString()
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			got, err := f.accounts.requestAccountDeletion(context.Background(), s.Token, id, func() time.Time { return f.now })
			if err != nil || got.RequestID != id {
				t.Error("retry failed", err)
			}
		}()
	}
	group.Wait()
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_account_deletions`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate deletion requests", err)
	}
}

func TestMySQLAccountDeletionRacingLoginCannotLeaveUsableSession(t *testing.T) {
	f := newPushFixture(t)
	s, _ := f.login(t, "racing-owner", f.now)
	ctx := context.Background()
	start := make(chan struct{})
	var group sync.WaitGroup
	tokens := make(chan string, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			grant, err := f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Subject: "racing-owner"}, RefreshToken: uuid.NewString()})
			if errors.Is(err, ErrOwner) {
				return
			}
			if err != nil {
				t.Error("racing registration", err)
				return
			}
			session, err := f.sessions.Issue(ctx, grant)
			if errors.Is(err, ErrSession) {
				return
			}
			if err != nil {
				t.Error("racing session", err)
				return
			}
			tokens <- session.Token
		}()
	}
	close(start)
	if _, err := f.accounts.requestAccountDeletion(ctx, s.Token, uuid.NewString(), func() time.Time { return f.now }); err != nil {
		t.Fatal(err)
	}
	group.Wait()
	close(tokens)
	for token := range tokens {
		if deletionAuthenticated(f, token) {
			t.Fatal("racing login left usable credential")
		}
	}
	var active int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_apple_grants WHERE account_id=? AND revoked_at IS NULL`, s.AccountID).Scan(&active); err != nil || active != 0 {
		t.Fatal("grant escaped cutover", err)
	}
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_apple_grants g LEFT JOIN album_deletion_apple_jobs j ON j.grant_id=g.grant_id WHERE g.account_id=? AND j.grant_id IS NULL`, s.AccountID).Scan(&active); err != nil || active != 0 {
		t.Fatal("racing grant escaped provider cleanup", err)
	}
}
