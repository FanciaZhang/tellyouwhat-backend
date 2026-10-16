package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"
)

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
