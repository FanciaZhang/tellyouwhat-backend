package migrations

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"

	"errors"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLConsentUpgradePreservesPublishedHistories(t *testing.T) {
	for _, history := range []struct {
		name, last, omitted, app, scope string
	}{
		{"published-health", "0066_privacy_consent_scopes.sql", "", "health", "guardian_consent"},
		{"deployed-journal", "0070_journal_read_aloud_consent.sql", "0066_privacy_consent_scopes.sql", "journal", "journal_read_aloud"},
	} {
		t.Run(history.name, func(t *testing.T) {
			db := migrationTestDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (name VARCHAR(255) PRIMARY KEY)`); err != nil {
				t.Fatal(err)
			}
			entries, err := fs.ReadDir(migrationFiles, ".")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				name := entry.Name()
				if !isMigrationFile(name) || name > history.last || name == history.omitted {
					continue
				}
				contents, err := migrationFiles.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, string(contents)); err != nil {
					t.Fatal(name, err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(name) VALUES(?)`, name); err != nil {
					t.Fatal(err)
				}
			}
			const device = "00000000-0000-4000-8000-000000000084"
			if _, err := db.ExecContext(ctx, `INSERT INTO app_attest_keys(app_id,key_id,device_id,public_key_der,environment,receipt) VALUES(?,'preserved',?,'fixture','development','fixture')`, history.app, device); err != nil {
				t.Fatal(err)
			}
			insert := func(scope string) error {
				_, err := db.ExecContext(ctx, `INSERT INTO privacy_consents(app_id,key_id,device_id,scope,document_version,granted,recorded_at) VALUES(?,'preserved',?,?,'fixture-v1',TRUE,'2026-11-01 01:02:03.123456')`, history.app, device, scope)
				return err
			}
			if err := insert(history.scope); err != nil {
				t.Fatal("cannot seed deployed history", err)
			}
			for range 2 {
				if err := Run(ctx, db); err != nil {
					t.Fatal("upgrade or replay lost existing consent", err)
				}
				var count int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM privacy_consents WHERE app_id=? AND key_id='preserved' AND device_id=? AND scope=? AND document_version='fixture-v1' AND granted=TRUE AND recorded_at='2026-11-01 01:02:03.123456'`, history.app, device, history.scope).Scan(&count); err != nil || count != 1 {
					t.Fatalf("existing consent changed: count=%d err=%v", count, err)
				}
				// Replaying committed DDL without its receipt must preserve both histories too.
				if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name IN ('0066_privacy_consent_scopes.sql','0067_journal_illustration_consent_scope.sql','0070_journal_read_aloud_consent.sql')`); err != nil {
					t.Fatal(err)
				}
			}
			var constraintError *mysql.MySQLError
			if err := insert("unknown_scope"); !errors.As(err, &constraintError) || constraintError.Number != 3819 {
				t.Fatalf("scope allowlist lost: %v", err)
			}
			var clause string
			if err := db.QueryRowContext(ctx, `SELECT CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS WHERE CONSTRAINT_SCHEMA=DATABASE() AND CONSTRAINT_NAME='privacy_consents_scope_chk'`).Scan(&clause); err != nil {
				t.Fatal(err)
			}
			for _, scope := range []string{"guardian_consent", "journal_illustration", "journal_read_aloud"} {
				if !strings.Contains(clause, scope) {
					t.Fatalf("current scope missing: %s", scope)
				}
			}
		})
	}
}
