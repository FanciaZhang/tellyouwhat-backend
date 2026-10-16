package albums

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

type pushFixture struct {
	dsn      string
	r        *albumPushRegistrations
	accounts *MySQLAccounts
	sessions *MySQLSessions
	now      time.Time
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	db, dsn := gatewayTestDatabase(t)
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal("migration replay", err)
	}
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{49}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	f := &pushFixture{dsn: dsn, now: time.Now().UTC().Truncate(time.Microsecond)}
	f.r, _ = newAlbumPushRegistrations(db, cipher, "production")
	f.accounts, _ = NewMySQLAccounts(albumDevelopmentAudience, db, cipher)
	f.accounts.now = func() time.Time { return f.now }
	f.sessions, _ = NewMySQLSessions(db, func() time.Time { return f.now })
	return f
}

func (f *pushFixture) login(t *testing.T, subject string, created time.Time) (AlbumSession, AccountLogin) {
	t.Helper()
	login, err := f.accounts.RegisterAppleLogin(context.Background(), AppleGrant{Identity: AppleIdentity{Audience: albumDevelopmentAudience, Subject: subject, IssuedAt: f.now}, RefreshToken: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.sessions.Issue(context.Background(), login)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := sessionDigest(s.Token)
	// Deterministic session ordering independent of database clock granularity.
	if _, err := f.r.db.Exec(`UPDATE album_sessions SET created_at=? WHERE token_sha256=?`, created, digest[:]); err != nil {
		t.Fatal(err)
	}
	return s, login
}

func pushUpdate(token string, revision int64) albumPushUpdate {
	return albumPushUpdate{RequestID: uuid.NewString(), ExpectedRevision: revision, Enabled: token != "", DeviceToken: token}
}

func TestMySQLAlbumPushEncryptionReplayExpiryAndLogout(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	s, _ := f.login(t, "owner-a", f.now)
	initial, err := f.r.current(ctx, s.Token, f.now)
	if err != nil || initial.ID != "" || initial.Revision != 0 || initial.State != "disabled" {
		t.Fatal("initial registration status", initial, err)
	}
	u := pushUpdate(strings.Repeat("ab", 32), 0)
	registered, err := f.r.update(ctx, s.Token, u, f.now)
	if err != nil || registered.State != "enabled" || registered.Revision != 1 {
		t.Fatal("register", registered, err)
	}
	again, err := f.r.update(ctx, s.Token, u, f.now.Add(time.Minute))
	if err != nil || again != registered {
		t.Fatal("response-loss replay", again, err)
	}
	if current, err := f.r.current(ctx, s.Token, f.now); err != nil || current != registered {
		t.Fatal("current registration status", current, err)
	}
	changed := u
	changed.DeviceToken = strings.Repeat("cd", 32)
	if _, err := f.r.update(ctx, s.Token, changed, f.now); !errors.Is(err, errPushRegistrationConflict) {
		t.Fatal("request ID reused", err)
	}
	reopened, _ := newAlbumPushRegistrations(f.r.db, f.r.cipher, "production")
	target, err := reopened.target(ctx, s.AccountID, registered.ID, 1)
	if err != nil || target.Token != u.DeviceToken || !target.RegisteredAt.Equal(f.now) {
		t.Fatal("reopened encrypted target", err)
	}
	encoded, _ := json.Marshal(target)
	for _, text := range []string{string(encoded), fmt.Sprint(target), fmt.Sprintf("%#v", target), fmt.Sprint(u), fmt.Sprintf("%#v", u)} {
		if strings.Contains(text, u.DeviceToken) {
			t.Fatal("device token exposed")
		}
	}
	var encrypted, nonce []byte
	var session string
	if err := f.r.db.QueryRow(`SELECT encrypted_token,token_nonce,session_id FROM album_push_registrations WHERE registration_id=?`, registered.ID).Scan(&encrypted, &nonce, &session); err != nil || bytes.Contains(encrypted, []byte(u.DeviceToken)) {
		t.Fatal("plaintext stored", err)
	}
	for _, aad := range [][]byte{
		pushTokenAAD(registered.ID, session, uuid.NewString(), "production", 1),
		pushTokenAAD(registered.ID, session, s.AccountID, "development", 1),
		pushTokenAAD(registered.ID, session, s.AccountID, "production", 2),
		pushTokenAAD(uuid.NewString(), session, s.AccountID, "production", 1),
	} {
		if _, err := f.r.cipher.Decrypt(encrypted, nonce, aad); err == nil {
			t.Fatal("ciphertext transplant accepted")
		}
	}
	f.now = s.ExpiresAt.Add(time.Second)
	if _, err := f.r.update(ctx, s.Token, pushUpdate(u.DeviceToken, 1), f.now); !errors.Is(err, ErrSession) {
		t.Fatal("expired session changed registration", err)
	}
	if _, err := f.r.target(ctx, s.AccountID, registered.ID, 1); err != nil {
		t.Fatal("normal session expiry lost future reminders", err)
	}
	if err := f.sessions.Revoke(ctx, s.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.current(ctx, s.Token, f.now); !errors.Is(err, ErrSession) {
		t.Fatal("revoked credential read registration", err)
	}
	if _, err := f.r.target(ctx, s.AccountID, registered.ID, 1); err == nil {
		t.Fatal("logout still eligible")
	}
}

func TestMySQLAlbumPushAccountSwitchFencesOldSession(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	old, _ := f.login(t, "owner-a", f.now.Add(-time.Hour))
	newer, _ := f.login(t, "owner-b", f.now)
	device := strings.Repeat("cd", 32)
	first, err := f.r.update(ctx, old.Token, pushUpdate(device, 0), f.now)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.r.update(ctx, newer.Token, pushUpdate(device, 0), f.now)
	if err != nil {
		t.Fatal("new account transfer", err)
	}
	if _, err := f.r.target(ctx, old.AccountID, first.ID, first.Revision); err == nil {
		t.Fatal("old account kept device")
	}
	if _, err := f.r.update(ctx, old.Token, pushUpdate(device, first.Revision+1), f.now); !errors.Is(err, errPushRegistrationConflict) {
		t.Fatal("old session resurrected registration", err)
	}
	if err := f.sessions.Revoke(ctx, old.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.target(ctx, newer.AccountID, current.ID, current.Revision); err != nil {
		t.Fatal("old logout disabled new account", err)
	}
	if _, err := f.r.target(ctx, old.AccountID, current.ID, current.Revision); err == nil {
		t.Fatal("foreign owner read token")
	}
	late, _ := f.login(t, "late-owner", f.now.Add(-2*time.Hour))
	if _, err := f.r.update(ctx, late.Token, pushUpdate(device, 0), f.now); !errors.Is(err, errPushRegistrationConflict) {
		t.Fatal("earlier session stole device", err)
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_push_registrations WHERE state='enabled'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("multiple owners for one endpoint", count, err)
	}
}

func TestMySQLAlbumPushRotationPermissionAndLateInvalidation(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	s, login := f.login(t, "owner", f.now)
	first, err := f.r.update(ctx, s.Token, pushUpdate("abcd", 0), f.now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.r.update(ctx, s.Token, pushUpdate("cdef", first.Revision), f.now.Add(time.Minute))
	if err != nil || first.ID != second.ID {
		t.Fatal("token rotation", err)
	}
	if err := f.r.invalidate(ctx, second.ID, second.Revision, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.target(ctx, s.AccountID, second.ID, second.Revision); err != nil {
		t.Fatal("older 410 disabled newer registration", err)
	}
	if err := f.r.invalidate(ctx, first.ID, first.Revision, f.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.target(ctx, s.AccountID, second.ID, second.Revision); err != nil {
		t.Fatal("stale generation disabled token", err)
	}
	disabled, err := f.r.update(ctx, s.Token, pushUpdate("", second.Revision), f.now)
	if err != nil || disabled.State != "disabled" {
		t.Fatal("permission withdrawal", err)
	}
	if _, err := f.r.target(ctx, s.AccountID, disabled.ID, disabled.Revision); err == nil {
		t.Fatal("disabled endpoint returned")
	}
	if _, err := f.r.update(ctx, s.Token, pushUpdate("cdef", second.Revision), f.now); !errors.Is(err, errPushRegistrationConflict) {
		t.Fatal("delayed enable undid permission withdrawal", err)
	}
	reenabled, err := f.r.update(ctx, s.Token, pushUpdate("cdef", disabled.Revision), f.now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.invalidate(ctx, reenabled.ID, reenabled.Revision, f.now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.target(ctx, s.AccountID, reenabled.ID, reenabled.Revision); err == nil {
		t.Fatal("current 410 ignored")
	}
	current, err := f.r.update(ctx, s.Token, pushUpdate("cdef", reenabled.Revision+1), f.now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.db.Exec(`UPDATE album_apple_grants SET revoked_at=? WHERE grant_id=?`, f.now, login.GrantID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.target(ctx, s.AccountID, current.ID, current.Revision); err == nil {
		t.Fatal("revoked Apple grant still eligible")
	}
}

func TestMySQLAlbumPushConcurrentCASAndEnvironmentIsolation(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	s, _ := f.login(t, "owner", f.now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes []albumPushRegistration
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.r.update(ctx, s.Token, pushUpdate("abcd", 0), f.now)
			if err == nil {
				mu.Lock()
				successes = append(successes, r)
				mu.Unlock()
			} else if !errors.Is(err, errPushRegistrationConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(successes) != 1 {
		t.Fatal("concurrent registrations committed", len(successes))
	}
	production := successes[0]
	development, _ := newAlbumPushRegistrations(f.r.db, f.r.cipher, "development")
	if _, err := development.target(ctx, s.AccountID, production.ID, 1); err == nil {
		t.Fatal("production token read through development")
	}
	dev, err := development.update(ctx, s.Token, pushUpdate("abcd", 0), f.now)
	if err != nil || dev.ID == production.ID {
		t.Fatal("separate signing environments collided", err)
	}
	if _, err := f.r.db.Exec(`UPDATE album_accounts SET state='disabled' WHERE account_id=?`, s.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.target(ctx, s.AccountID, production.ID, 1); err == nil {
		t.Fatal("disabled account still eligible")
	}
	if _, err := f.r.update(ctx, s.Token, pushUpdate("abcd", 1), f.now); !errors.Is(err, ErrSession) {
		t.Fatal("disabled account updated registration", err)
	}
}

func TestMySQLAlbumPushTransferFailureRollsBackOldBinding(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	old, _ := f.login(t, "old-owner", f.now.Add(-time.Hour))
	newer, _ := f.login(t, "new-owner", f.now)
	device := strings.Repeat("ef", 32)
	first, err := f.r.update(ctx, old.Token, pushUpdate(device, 0), f.now)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := sessionDigest(newer.Token)
	var session string
	if err := f.r.db.QueryRow(`SELECT session_id FROM album_sessions WHERE token_sha256=?`, digest[:]).Scan(&session); err != nil || !validOwner(session) {
		t.Fatal(err)
	}
	// Fail after the old binding is superseded, without triggers or global privileges.
	if _, err := f.r.db.Exec(`ALTER TABLE album_push_registrations ADD CONSTRAINT reject_new_push CHECK(state<>'enabled' OR session_id<>'` + session + `')`); err != nil {
		t.Fatal(err)
	}
	request := pushUpdate(device, 0)
	if _, err := f.r.update(ctx, newer.Token, request, f.now); !errors.Is(err, errPushRegistration) {
		t.Fatal("partial transfer committed", err)
	}
	if _, err := f.r.target(ctx, old.AccountID, first.ID, first.Revision); err != nil {
		t.Fatal("failed transfer destroyed old binding", err)
	}
	var rows int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_push_registrations`).Scan(&rows); err != nil || rows != 1 {
		t.Fatal("partial new binding remains", rows, err)
	}
	if _, err := f.r.db.Exec(`ALTER TABLE album_push_registrations DROP CHECK reject_new_push`); err != nil {
		t.Fatal(err)
	}
	current, err := f.r.update(ctx, newer.Token, request, f.now)
	if err != nil {
		t.Fatal("same request could not recover", err)
	}
	if _, err := f.r.target(ctx, newer.AccountID, current.ID, current.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.target(ctx, old.AccountID, first.ID, first.Revision); err == nil {
		t.Fatal("old binding survived successful transfer")
	}
}
