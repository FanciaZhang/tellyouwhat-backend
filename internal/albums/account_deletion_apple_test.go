package albums

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func acceptAppleDeletion(t *testing.T, f *pushFixture, token string) accountDeletionRequest {
	t.Helper()
	receipt, err := f.accounts.requestAccountDeletion(context.Background(), token, uuid.NewString(), func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func assertAppleCredential(t *testing.T, f *pushFixture, g AccountLogin, retained bool) {
	t.Helper()
	token, err := f.accounts.RefreshCredential(context.Background(), g.AccountID, g.GrantID)
	if retained && (err != nil || token == "") || !retained && err == nil {
		t.Fatal("unexpected credential retention", retained, err)
	}
	var size int
	if err := f.r.db.QueryRow(`SELECT OCTET_LENGTH(encrypted_token) FROM album_apple_grants WHERE grant_id=?`, g.GrantID).Scan(&size); err != nil || (size > 0) != retained {
		t.Fatal("unexpected ciphertext retention", size, err)
	}
}

func TestMySQLAppleDeletionRetriesEveryGrantAndPreservesOtherAccount(t *testing.T) {
	f := newPushFixture(t)
	s, first := f.login(t, "delete", f.now)
	_, older := f.login(t, "delete", f.now)
	other, otherGrant := f.login(t, "keep", f.now)
	if _, err := f.r.db.Exec(`UPDATE album_apple_grants SET revoked_at=? WHERE grant_id=?`, f.now, older.GrantID); err != nil {
		t.Fatal(err)
	}
	acceptAppleDeletion(t, f, s.Token)
	ctx := context.Background()
	calls := 0
	fail := func(context.Context, string) error { calls++; return errors.New("private provider failure") }
	clock := func() time.Time { return f.now }
	b, err := f.accounts.runAppleDeletionBatch(ctx, fail, 10, clock)
	if err != nil || b.Deferred != 2 || calls != 2 {
		t.Fatal("all grants were not deferred", b, err)
	}
	assertAppleCredential(t, f, first, true)
	assertAppleCredential(t, f, older, true)
	if b, err = f.accounts.runAppleDeletionBatch(ctx, fail, 10, clock); err != nil || b != (appleDeletionBatch{}) || calls != 2 {
		t.Fatal("backoff ignored", b, err)
	}
	reopened, _ := NewMySQLAccounts(albumDevelopmentAudience, f.r.db, f.r.cipher)
	success := func(context.Context, string) error { calls++; return nil }
	b, err = reopened.runAppleDeletionBatch(ctx, success, 10, func() time.Time { return f.now.Add(time.Minute) })
	if err != nil || b.Acknowledged != 2 || calls != 4 {
		t.Fatal("restart did not finish", b, err)
	}
	assertAppleCredential(t, f, first, false)
	assertAppleCredential(t, f, older, false)
	assertAppleCredential(t, f, otherGrant, true)
	if !deletionAuthenticated(f, other.Token) {
		t.Fatal("unrelated account affected")
	}
	if b, err = reopened.runAppleDeletionBatch(ctx, success, 10, func() time.Time { return f.now.Add(24 * time.Hour) }); err != nil || b != (appleDeletionBatch{}) || calls != 4 {
		t.Fatal("completed work ran again", b, err)
	}
	var acknowledged int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_apple_jobs WHERE acknowledged_at IS NOT NULL AND lease_token IS NULL AND attempts=2`).Scan(&acknowledged); err != nil || acknowledged != 2 {
		t.Fatal("missing durable acknowledgements", err)
	}
}

func TestMySQLAppleDeletionLeaseRecoveryRejectsStaleWorker(t *testing.T) {
	f := newPushFixture(t)
	s, g := f.login(t, "delete", f.now)
	d := acceptAppleDeletion(t, f, s.Token)
	ctx := context.Background()
	ref := appleDeletionJob{account: s.AccountID, request: d.RequestID, grant: g.GrantID}
	first, err := f.accounts.claimAppleDeletion(ctx, ref, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.accounts.claimAppleDeletion(ctx, ref, f.now.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("live lease stolen", err)
	}
	if err = f.accounts.finishAppleDeletion(ctx, first, true, f.now.Add(appleDeletionLease)); !errors.Is(err, ErrConflict) {
		t.Fatal("expired worker erased credential", err)
	}
	assertAppleCredential(t, f, g, true)
	reopened, _ := NewMySQLAccounts(albumDevelopmentAudience, f.r.db, f.r.cipher)
	second, err := reopened.claimAppleDeletion(ctx, ref, f.now.Add(appleDeletionLease))
	if err != nil || second.lease == first.lease {
		t.Fatal("abandoned lease not recovered", err)
	}
	if err = f.accounts.finishAppleDeletion(ctx, first, true, f.now.Add(appleDeletionLease)); !errors.Is(err, ErrConflict) {
		t.Fatal("superseded worker acknowledged", err)
	}
	if err = reopened.finishAppleDeletion(ctx, second, true, f.now.Add(appleDeletionLease+time.Second)); err != nil {
		t.Fatal(err)
	}
	assertAppleCredential(t, f, g, false)
}

func TestMySQLAppleDeletionAcknowledgementFailureRollsBackCredentialWipe(t *testing.T) {
	f := newPushFixture(t)
	s, g := f.login(t, "delete", f.now)
	acceptAppleDeletion(t, f, s.Token)
	ctx := context.Background()
	calls := 0
	provider := func(context.Context, string) error {
		calls++
		// Force the credential update to fail after the acknowledgement UPDATE.
		_, err := f.r.db.Exec(`ALTER TABLE album_apple_grants RENAME COLUMN token_nonce TO unavailable_nonce`)
		return err
	}
	if _, err := f.accounts.runAppleDeletionBatch(ctx, provider, 1, func() time.Time { return f.now }); err == nil {
		t.Fatal("partial completion accepted")
	}
	if _, err := f.r.db.Exec(`ALTER TABLE album_apple_grants RENAME COLUMN unavailable_nonce TO token_nonce`); err != nil {
		t.Fatal(err)
	}
	assertAppleCredential(t, f, g, true)
	var ack sql.NullTime
	if err := f.r.db.QueryRow(`SELECT acknowledged_at FROM album_deletion_apple_jobs WHERE grant_id=?`, g.GrantID).Scan(&ack); err != nil || ack.Valid {
		t.Fatal("partial acknowledgement committed", err)
	}
	reopened, _ := NewMySQLAccounts(albumDevelopmentAudience, f.r.db, f.r.cipher)
	b, err := reopened.runAppleDeletionBatch(ctx, func(context.Context, string) error { calls++; return nil }, 1, func() time.Time { return f.now.Add(appleDeletionLease) })
	if err != nil || b.Acknowledged != 1 || calls != 2 {
		t.Fatal("unknown outcome not safely retried", b, err)
	}
	assertAppleCredential(t, f, g, false)
}

func TestMySQLAppleDeletionCancellationLeavesRecoverableLease(t *testing.T) {
	f := newPushFixture(t)
	s, g := f.login(t, "delete", f.now)
	acceptAppleDeletion(t, f, s.Token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := func(call context.Context, token string) error {
		if deadline, ok := call.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
			t.Fatal("provider call not bounded")
		}
		cancel()
		return nil
	}
	if _, err := f.accounts.runAppleDeletionBatch(ctx, provider, 1, func() time.Time { return f.now }); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	assertAppleCredential(t, f, g, true)
	b, err := f.accounts.runAppleDeletionBatch(context.Background(), func(context.Context, string) error { return nil }, 1, func() time.Time { return f.now.Add(appleDeletionLease) })
	if err != nil || b.Acknowledged != 1 {
		t.Fatal("cancelled work not recoverable", b, err)
	}
}

func TestMySQLAppleDeletionConcurrentWorkersClaimOnce(t *testing.T) {
	f := newPushFixture(t)
	s, g := f.login(t, "delete", f.now)
	acceptAppleDeletion(t, f, s.Token)
	var calls atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.accounts.runAppleDeletionBatch(context.Background(), func(context.Context, string) error { calls.Add(1); return nil }, 10, func() time.Time { return f.now })
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate provider calls", calls.Load())
	}
	assertAppleCredential(t, f, g, false)
}

func TestMySQLAppleDeletionMigrationBackfillsAcceptedRequests(t *testing.T) {
	f := newPushFixture(t)
	s, g := f.login(t, "delete", f.now)
	_, _ = f.login(t, "keep", f.now)
	acceptAppleDeletion(t, f, s.Token)
	if _, err := f.r.db.Exec(`DROP TABLE album_deletion_apple_jobs`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../migrations/0042_album_deletion_apple_jobs.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range strings.Split(string(data), ";") {
		if strings.TrimSpace(query) != "" {
			if _, err := f.r.db.Exec(query); err != nil {
				t.Fatal(err)
			}
		}
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_apple_jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatal("wrong migrated jobs", err)
	}
	assertAppleCredential(t, f, g, true)
	b, err := f.accounts.runAppleDeletionBatch(context.Background(), func(context.Context, string) error { return nil }, 10, func() time.Time { return f.now })
	if err != nil || b.Acknowledged != 1 {
		t.Fatal("migrated work not executable", b, err)
	}
}

func TestAppleDeletionRetryIsBounded(t *testing.T) {
	for attempt, want := range map[int]time.Duration{0: time.Minute, 1: time.Minute, 2: 2 * time.Minute, 9: 256 * time.Minute, 10: 6 * time.Hour, 1000000: 6 * time.Hour} {
		if got := accountDeletionRetry(attempt); got != want {
			t.Fatal(attempt, got, want)
		}
	}
}

func TestMySQLAppleDeletionRejectsWrongReceiptAndUnreadableCredential(t *testing.T) {
	f := newPushFixture(t)
	s, g := f.login(t, "delete", f.now)
	d := acceptAppleDeletion(t, f, s.Token)
	ctx := context.Background()
	ref := appleDeletionJob{account: s.AccountID, request: uuid.NewString(), grant: g.GrantID}
	if _, err := f.accounts.claimAppleDeletion(ctx, ref, f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong deletion receipt accepted", err)
	}
	ref.request = d.RequestID
	var encrypted []byte
	if err := f.r.db.QueryRow(`SELECT encrypted_token FROM album_apple_grants WHERE grant_id=?`, g.GrantID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.db.Exec(`UPDATE album_apple_grants SET encrypted_token=X'00' WHERE grant_id=?`, g.GrantID); err != nil {
		t.Fatal(err)
	}
	called := false
	provider := func(context.Context, string) error { called = true; return nil }
	b, err := f.accounts.runAppleDeletionBatch(ctx, provider, 1, func() time.Time { return f.now })
	if err != nil || b.Deferred != 1 || called {
		t.Fatal("unreadable token was sent or treated as revoked", b, err)
	}
	if _, err := f.r.db.Exec(`UPDATE album_apple_grants SET encrypted_token=? WHERE grant_id=?`, encrypted, g.GrantID); err != nil {
		t.Fatal(err)
	}
	b, err = f.accounts.runAppleDeletionBatch(ctx, provider, 1, func() time.Time { return f.now.Add(time.Minute) })
	if err != nil || b.Acknowledged != 1 || !called {
		t.Fatal("credential recovery did not resume task", b, err)
	}
}
