package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRunningGatewayStopsAdmissionWhenRecoveryStateChanges(t *testing.T) {
	f := newAppleGatewayFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g, err := NewGateway(ctx, f.config, f.transport(t, func(*http.Request) (*http.Response, error) {
		t.Error("unexpected Apple request")
		return nil, errors.New("unexpected Apple request")
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	request := func(method, path string, want int) {
		t.Helper()
		out := httptest.NewRecorder()
		g.ServeHTTP(out, httptest.NewRequest(method, "https://"+f.config.APIHost+path, nil))
		if out.Code != want || want == http.StatusServiceUnavailable && out.Body.String() != `{"code":"not_ready"}` {
			t.Errorf("%s %s: got %d, want %d", method, path, out.Code, want)
		}
	}
	assertChallenges := func(want int) {
		t.Helper()
		var count int
		if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_login_challenges`).Scan(&count); err != nil || count != want {
			t.Errorf("challenge rows: got %d, want %d; %v", count, want, err)
		}
	}
	request(http.MethodGet, "/readyz", http.StatusOK)
	request(http.MethodPost, "/v1/albums/auth/challenges", http.StatusCreated)
	assertChallenges(1)
	started := 0
	if !g.runBackgroundBatch(ctx, func() { started++ }) || started != 1 {
		t.Fatal("active database did not admit background work")
	}
	for _, statement := range []string{
		`UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=UTC_TIMESTAMP(6) WHERE singleton_id=1`,
		`DELETE FROM album_recovery_state WHERE singleton_id=1`,
		`RENAME TABLE album_recovery_state TO album_hidden_recovery_state`,
	} {
		if _, err := f.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
		request(http.MethodGet, "/livez", http.StatusOK)
		request(http.MethodGet, "/readyz", http.StatusServiceUnavailable)
		request(http.MethodPost, "/v1/albums/auth/challenges", http.StatusServiceUnavailable)
		assertChallenges(1)
		if !g.runBackgroundBatch(ctx, func() { started++ }) || started != 1 {
			t.Fatal("unavailable database admitted background work or stopped the poller")
		}
	}
}

func TestGatewayRecoveryAdmissionBeforeRedisAndProviders(t *testing.T) {
	f := newAppleGatewayFixture(t)
	f.config.RedisURL = "invalid-redis-url"
	transport := appleTestTransport(func(*http.Request) (*http.Response, error) {
		t.Error("quarantine admission reached provider")
		return nil, errors.New("unexpected provider")
	})
	for _, step := range []struct{ name, sql, want string }{
		{"fresh", "", "album Redis configuration"},
		{"quarantined", `UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=UTC_TIMESTAMP(6)`, "album recovery state unavailable or quarantined"},
		{"missing row", `DELETE FROM album_recovery_state`, "album recovery state unavailable or quarantined"},
		{"missing table", `DROP TABLE album_recovery_state`, "album recovery state unavailable or quarantined"},
	} {
		t.Run(step.name, func(t *testing.T) {
			if step.sql != "" {
				if _, err := f.db.Exec(step.sql); err != nil {
					t.Fatal(err)
				}
			}
			g, err := NewGateway(context.Background(), f.config, transport)
			if g != nil {
				g.Close()
				t.Fatal("gateway admitted")
			}
			if err == nil || err.Error() != step.want {
				t.Fatalf("unexpected admission result: %v", err)
			}
		})
	}
}

func TestGatewayRecoveryAdmissionPrecedesAttestationMiddleware(t *testing.T) {
	f := newAppleGatewayFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g, err := NewGateway(ctx, f.config, f.transport(t, func(*http.Request) (*http.Response, error) {
		t.Error("unexpected Apple request")
		return nil, errors.New("unexpected Apple request")
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	const challenge, registration = "/v1/attest/challenges", "/v1/attest/keys"
	calls := 0
	bootstrap := g.GuardAttestationBootstrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
	}))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == challenge || r.URL.Path == registration {
			bootstrap.ServeHTTP(w, r)
			return
		}
		g.ServeHTTP(w, r)
	})
	request := func(method, path string, want int) {
		t.Helper()
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(method, "https://"+f.config.APIHost+path, nil))
		if out.Code != want || want == http.StatusServiceUnavailable && out.Body.String() != `{"code":"not_ready"}` {
			t.Errorf("%s %s: got %d, want %d", method, path, out.Code, want)
		}
	}
	request(http.MethodPost, challenge, http.StatusCreated)
	request(http.MethodGet, "/readyz", http.StatusOK)
	if _, err := f.db.ExecContext(ctx, `UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=UTC_TIMESTAMP(6)`); err != nil {
		t.Fatal(err)
	}
	request(http.MethodPost, challenge, http.StatusServiceUnavailable)
	request(http.MethodPost, registration, http.StatusServiceUnavailable)
	request(http.MethodGet, "/readyz", http.StatusServiceUnavailable)
	request(http.MethodGet, "/livez", http.StatusOK)
	if _, err := f.db.ExecContext(ctx, `UPDATE album_recovery_state SET quarantined=FALSE,quarantined_at=NULL`); err != nil {
		t.Fatal(err)
	}
	request(http.MethodPost, registration, http.StatusCreated)
	g.BeginDrain()
	request(http.MethodPost, challenge, http.StatusServiceUnavailable)
	request(http.MethodPost, registration, http.StatusServiceUnavailable)
	request(http.MethodGet, "/livez", http.StatusOK)
	if calls != 2 {
		t.Errorf("attestation middleware reached %d times, want 2", calls)
	}
}

func TestMySQLRecoveryGuardsRejectStaleSnapshotAndMissingState(t *testing.T) {
	f := newSharingFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := CheckRecoveryState(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	// Establish a pre-quarantine consistent snapshot. Locking guards must see
	// the current marker rather than this transaction's old read view.
	tx, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT quarantined FROM album_recovery_state`).Scan(&blocked); err != nil || blocked {
		t.Fatal("initial snapshot", err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=UTC_TIMESTAMP(6)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT quarantined FROM album_recovery_state`).Scan(&blocked); err != nil || blocked {
		t.Fatal("fixture lost stale snapshot", err)
	}
	if _, err := lockAccount(ctx, tx, f.login.AccountID); !errors.Is(err, ErrRecoveryQuarantined) {
		t.Fatal("storage bypassed quarantine", err)
	}
	if err := requireMediaAccount(ctx, tx, f.login.AccountID); !errors.Is(err, ErrRecoveryQuarantined) {
		t.Fatal("media bypassed quarantine", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := CheckRecoveryState(ctx, f.db); !errors.Is(err, ErrRecoveryQuarantined) {
		t.Fatal("startup bypassed quarantine", err)
	}
	if _, err := f.db.ExecContext(ctx, `DELETE FROM album_recovery_state`); err != nil {
		t.Fatal(err)
	}
	tx, err = f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, f.login.AccountID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing marker allowed storage access", err)
	}
	if err := requireMediaAccount(ctx, tx, f.login.AccountID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing marker allowed media access", err)
	}
}

func TestMySQLRecoverySharedAdmissionDoesNotSerializeWriters(t *testing.T) {
	f := newSharingFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	if err := lockRecoveryState(ctx, first); err != nil {
		t.Fatal(err)
	}
	second, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback()
	if err := lockRecoveryState(ctx, second); err != nil {
		t.Fatal("unrelated transaction blocked", err)
	}
}
