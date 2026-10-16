package albums

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func identityFenceCount(t *testing.T, f *pushFixture, subject string) int {
	t.Helper()
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_apple_identity_state WHERE subject_sha256=?`, appleSubjectHash(albumDevelopmentAudience, subject)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestMySQLIdentityRetirementAfterAccountErasureRejectsDelayedLogin(t *testing.T) {
	x := newAccessErasureFixture(t, true, true)
	r := readyRecordErasure(t, x)
	ctx := context.Background()
	clock := func() time.Time { return x.f.now }
	issued := x.f.now
	if _, err := r.eraseAccountRecords(ctx, x.d.AccountID, x.d.RequestID, clock); err != nil {
		t.Fatal(err)
	}
	x.f.now = issued.Add(identityFenceRetention)
	if count, err := r.runIdentityRetirementBatch(ctx, 10, clock); err != nil || count != 0 || identityFenceCount(t, x.f, "erasure-owner") != 1 {
		t.Fatal("retired before complete assertion window", count, err)
	}
	x.f.now = x.f.now.Add(time.Microsecond)
	if count, err := r.runIdentityRetirementBatch(ctx, 10, clock); err != nil || count != 1 || identityFenceCount(t, x.f, "erasure-owner") != 0 {
		t.Fatal("orphan identity retained", count, err)
	}
	for _, iat := range []time.Time{time.Time{}, issued, issued.Add(appleIdentityClockSkew), x.f.now.Add(appleIdentityClockSkew + time.Second)} {
		if _, err := x.f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Audience: albumDevelopmentAudience, Subject: "erasure-owner", IssuedAt: iat}, RefreshToken: uuid.NewString()}); !errors.Is(err, ErrAppleCode) {
			t.Fatal("invalid or stale assertion recreated an erased identity", iat, err)
		}
		if identityFenceCount(t, x.f, "erasure-owner") != 0 {
			t.Fatal("failed registration left an identity record")
		}
	}
	if receipt, err := x.f.accounts.requestAccountDeletion(ctx, x.first.Token, x.d.RequestID, clock); err != nil || receipt != x.d {
		t.Fatal("identity retirement erased independent deletion intent", err)
	}
	login, err := x.f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Audience: albumDevelopmentAudience, Subject: "erasure-owner", IssuedAt: x.f.now}, RefreshToken: uuid.NewString()})
	if err != nil || login.AccountID == x.d.AccountID {
		t.Fatal("fresh reauthorization failed", err)
	}
	if count, err := r.runIdentityRetirementBatch(ctx, 10, clock); err != nil || count != 0 || identityFenceCount(t, x.f, "erasure-owner") != 1 {
		t.Fatal("recreated identity retired", count, err)
	}
}

func TestMySQLIdentityRetirementKeepsLinkedOrRenewedFences(t *testing.T) {
	for _, mode := range []string{"active", "deleting", "consent-revoked", "account-deleted", "quarantined", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			f := newPushFixture(t)
			ctx := context.Background()
			r := NewMySQLUploads(f.r.db).WithSubscriptionEnvironment("Sandbox")
			subject := "identity-" + mode
			var session AlbumSession
			if mode == "active" || mode == "deleting" {
				session, _ = f.login(t, subject, f.now)
			}
			old := f.now.Add(-2 * identityFenceRetention)
			if err := f.accounts.applyAppleEvent(ctx, appleAccountEvent{Audience: albumDevelopmentAudience, Type: "consent-revoked", Subject: subject, EventTime: old.Unix()}); err != nil {
				t.Fatal(err)
			}
			if mode == "deleting" {
				acceptAppleDeletion(t, f, session.Token)
			}
			if mode == "consent-revoked" || mode == "account-deleted" {
				// A candidate selected before this notification must recheck the row.
				if err := f.accounts.applyAppleEvent(ctx, appleAccountEvent{Audience: albumDevelopmentAudience, Type: mode, Subject: subject, EventTime: f.now.Unix()}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "quarantined" {
				if _, err := f.r.db.Exec(`UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=? WHERE singleton_id=1`, f.now); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "rollback" {
				if _, err := f.r.db.Exec(`CREATE TABLE identity_retirement_guard(subject_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY, FOREIGN KEY(subject_sha256) REFERENCES album_apple_identity_state(subject_sha256))`); err != nil {
					t.Fatal(err)
				}
				if _, err := f.r.db.Exec(`INSERT INTO identity_retirement_guard VALUES (?)`, appleSubjectHash(albumDevelopmentAudience, subject)); err != nil {
					t.Fatal(err)
				}
			}
			removed, err := r.retireIdentityFence(ctx, appleSubjectHash(albumDevelopmentAudience, subject), func() time.Time { return f.now })
			if removed || (mode == "quarantined" && !errors.Is(err, ErrRecoveryQuarantined)) || (mode == "rollback" && err == nil) {
				t.Fatal("unsafe identity retirement", removed, err)
			}
			if err != nil && mode != "quarantined" && mode != "rollback" {
				t.Fatal(err)
			}
			if identityFenceCount(t, f, subject) != 1 {
				t.Fatal("protected identity was removed")
			}
			if mode == "rollback" {
				if _, err := f.r.db.Exec(`DROP TABLE identity_retirement_guard`); err != nil {
					t.Fatal(err)
				}
				if removed, err := r.retireIdentityFence(ctx, appleSubjectHash(albumDevelopmentAudience, subject), func() time.Time { return f.now }); err != nil || !removed {
					t.Fatal("retry after failed retirement", removed, err)
				}
			}
		})
	}
}

func TestMySQLIdentityRetirementSerializesWithFreshRegistration(t *testing.T) {
	f := newPushFixture(t)
	r := NewMySQLUploads(f.r.db).WithSubscriptionEnvironment("Sandbox")
	ctx := context.Background()
	for _, subject := range []string{"race-one", "race-two", "race-three"} {
		if err := f.accounts.applyAppleEvent(ctx, appleAccountEvent{Audience: albumDevelopmentAudience, Type: "consent-revoked", Subject: subject, EventTime: f.now.Add(-2 * identityFenceRetention).Unix()}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := r.retireIdentityFence(ctx, appleSubjectHash(albumDevelopmentAudience, subject), func() time.Time { return f.now }); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Audience: albumDevelopmentAudience, Subject: subject, IssuedAt: f.now}, RefreshToken: uuid.NewString()}); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		if identityFenceCount(t, f, subject) != 1 {
			t.Fatal("concurrent fresh account lost its identity state")
		}
	}
}

func TestMySQLAppleRegistrationRechecksAssertionAfterIdentityLock(t *testing.T) {
	f := newPushFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	subject := "delayed-identity-lock"
	if err := f.accounts.applyAppleEvent(ctx, appleAccountEvent{Audience: albumDevelopmentAudience, Type: "consent-revoked", Subject: subject, EventTime: f.now.Add(-time.Minute).Unix()}); err != nil {
		t.Fatal(err)
	}
	hold, err := f.r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	var hash string
	if err = hold.QueryRowContext(ctx, `SELECT subject_sha256 FROM album_apple_identity_state WHERE subject_sha256=? FOR UPDATE`, appleSubjectHash(albumDevelopmentAudience, subject)).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	var released atomic.Bool
	f.accounts.now = func() time.Time {
		if !released.Load() {
			t.Error("assertion age checked before identity lock")
		}
		return f.now.Add(2 * identityFenceRetention)
	}
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		_, err := f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Audience: albumDevelopmentAudience, Subject: subject, IssuedAt: f.now}, RefreshToken: uuid.NewString()})
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatal("registration bypassed held identity lock", err)
	case <-time.After(100 * time.Millisecond):
	}
	released.Store(true)
	if err = hold.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrAppleCode) {
		t.Fatal("expired pre-wait assertion accepted", err)
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_accounts WHERE apple_subject_sha256=?`, hash).Scan(&count); err != nil || count != 0 {
		t.Fatal("delayed login created an account", count, err)
	}
}
