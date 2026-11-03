package albums

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMySQLAccountDeletionWithdrawalRevokesOnlyOriginalCredentialAndStopsLateAcceptance(t *testing.T) {
	for _, mode := range []string{"active", "expired", "revoked", "disabled", "missing-session"} {
		t.Run(mode, func(t *testing.T) {
			f := newPushFixture(t)
			first, _ := f.login(t, "owner", f.now)
			second, _ := f.login(t, "owner", f.now)
			other, _ := f.login(t, "other", f.now)
			ctx := context.Background()
			id := uuid.NewString()
			if mode == "missing-session" {
				if _, err := f.r.db.Exec(`DELETE FROM album_sessions WHERE token_sha256=?`, mustSessionDigest(t, first.Token)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "expired" {
				_, err := f.r.db.Exec(`UPDATE album_sessions SET expires_at=? WHERE token_sha256=?`, f.now, mustSessionDigest(t, first.Token))
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "revoked" {
				if err := f.sessions.Revoke(ctx, first.Token); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "disabled" {
				if _, err := f.r.db.Exec(`UPDATE album_accounts SET state='disabled' WHERE account_id=?`, first.AccountID); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				receipt, err := f.accounts.withdrawAccountDeletion(ctx, first.Token, id, func() time.Time { return f.now })
				if err != nil || receipt != nil {
					t.Fatal(receipt, err)
				}
			}
			if _, err := f.accounts.requestAccountDeletion(ctx, first.Token, id, func() time.Time { return f.now }); !errors.Is(err, ErrSession) {
				t.Fatal("late request accepted", err)
			}
			if deletionAuthenticated(f, first.Token) || !deletionAuthenticated(f, other.Token) || mode != "disabled" && !deletionAuthenticated(f, second.Token) {
				t.Fatal("wrong credential revoked")
			}
			var n int
			if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_ledger`).Scan(&n); err != nil || n != 0 {
				t.Fatal("withdrawal created deletion intent", n, err)
			}
		})
	}
}

func mustSessionDigest(t *testing.T, token string) []byte {
	t.Helper()
	d, e := sessionDigest(token)
	if e != nil {
		t.Fatal(e)
	}
	return d[:]
}

func TestMySQLAccountDeletionWithdrawalPreservesAcceptedIntentAfterAccountRowsErased(t *testing.T) {
	x := newAccessErasureFixture(t, true, true)
	ctx := context.Background()
	if _, err := x.f.accounts.withdrawAccountDeletion(ctx, x.first.Token, uuid.NewString(), func() time.Time { return x.f.now }); !errors.Is(err, ErrSession) {
		t.Fatal("different request withdrew accepted deletion", err)
	}

	if _, err := x.f.accounts.eraseAccountAccess(ctx, x.d.AccountID, x.d.RequestID, x.f.now); err != nil {
		t.Fatal(err)
	}
	// Exercise the independent receipt after removal of synthetic live records.
	// This is not an implementation of the final identity erasure stage.
	for _, q := range []string{`DELETE FROM album_account_object_purges WHERE account_id=?`, `DELETE FROM album_account_deletions WHERE account_id=?`, `DELETE FROM album_accounts WHERE account_id=?`} {
		if _, err := x.f.r.db.Exec(q, x.d.AccountID); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := x.f.accounts.withdrawAccountDeletion(ctx, x.first.Token, x.d.RequestID, func() time.Time { return x.f.now })
	if err != nil || receipt == nil || *receipt != x.d {
		t.Fatal("accepted intent lost", receipt, err)
	}
	replay, err := x.f.accounts.requestAccountDeletion(ctx, x.first.Token, x.d.RequestID, func() time.Time { return x.f.now })
	if err != nil || replay != x.d {
		t.Fatal("replay lost", err)
	}
	if _, err = x.f.accounts.withdrawAccountDeletion(ctx, x.other.Token, x.d.RequestID, func() time.Time { return x.f.now }); !errors.Is(err, ErrSession) {
		t.Fatal("foreign receipt disclosed", err)
	}
	if !deletionAuthenticated(x.f, x.other.Token) {
		t.Fatal("foreign session changed")
	}
}

func TestMySQLAccountDeletionWithdrawalAndAcceptanceSerialize(t *testing.T) {
	for range 8 {
		f := newPushFixture(t)
		s, _ := f.login(t, "owner", f.now)
		id := uuid.NewString()
		ctx := context.Background()
		var wg sync.WaitGroup
		wg.Add(2)
		var accepted accountDeletionRequest
		var acceptErr, withdrawErr error
		var withdrawal *accountDeletionRequest
		go func() {
			defer wg.Done()
			accepted, acceptErr = f.accounts.requestAccountDeletion(ctx, s.Token, id, func() time.Time { return f.now })
		}()
		go func() {
			defer wg.Done()
			withdrawal, withdrawErr = f.accounts.withdrawAccountDeletion(ctx, s.Token, id, func() time.Time { return f.now })
		}()
		wg.Wait()
		if withdrawErr != nil {
			t.Fatal(withdrawErr)
		}
		if withdrawal == nil {
			if !errors.Is(acceptErr, ErrSession) {
				t.Fatal("withdrawal raced with acceptance", acceptErr)
			}
		} else if acceptErr != nil || *withdrawal != accepted {
			t.Fatal("accepted request reported withdrawn", acceptErr)
		}
		retry, err := f.accounts.requestAccountDeletion(ctx, s.Token, id, func() time.Time { return f.now })
		if withdrawal == nil && !errors.Is(err, ErrSession) || withdrawal != nil && (err != nil || retry != *withdrawal) {
			t.Fatal("late retry changed outcome", err)
		}
	}
}

func TestMySQLAccountDeletionWithdrawalHTTPAndFailureRollback(t *testing.T) {
	f := newPushFixture(t)
	s, _ := f.login(t, "owner", f.now)
	id := uuid.NewString()
	body := `{"requestID":"` + id + `"}`
	path := deletionPath + "/withdrawal"
	ctx := context.Background()
	p, err := f.r.update(ctx, s.Token, pushUpdate(strings.Repeat("ab", 32), 0), f.now)
	if err != nil {
		t.Fatal(err)
	}
	limiter := &loginHTTPLimiter{}
	router := deletionRouter(t, f, limiter, true)
	if w := sendDeletion(deletionRouter(t, f, limiter, false), "POST", path, s.Token, body, nil); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if _, err = f.r.db.Exec(`ALTER TABLE album_push_registrations ADD CONSTRAINT fail_withdrawal_push CHECK (revision=1)`); err != nil {
		t.Fatal(err)
	}
	if w := sendDeletion(router, "POST", path, s.Token, body, nil); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if !deletionAuthenticated(f, s.Token) {
		t.Fatal("failed transaction revoked session")
	}
	if _, err = f.r.db.Exec(`ALTER TABLE album_push_registrations DROP CHECK fail_withdrawal_push`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		w := sendDeletion(router, "POST", path, s.Token, body, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"withdrawn"`) || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if _, err = f.r.target(ctx, s.AccountID, p.ID, p.Revision); err == nil {
		t.Fatal("push binding survived withdrawal")
	}
	if w := sendDeletion(router, "POST", deletionPath, s.Token, body, nil); w.Code != 401 {
		t.Fatal("late acceptance", w.Code)
	}
	other, _ := f.login(t, "accepted-owner", f.now)
	accepted := acceptAppleDeletion(t, f, other.Token)
	w := sendDeletion(router, "POST", path, other.Token, `{"requestID":"`+accepted.RequestID+`"}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"accepted"`) {
		t.Fatal("accepted request cancelled", w.Code)
	}
}
