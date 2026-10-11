package migrations

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func migrationTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_ADMIN_DSN required for real migration tests")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(config.DBName, "_test") {
		t.Fatal("refusing non-test database")
	}
	config.ParseTime, config.MultiStatements = true, true
	admin, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	name := "album_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "") + "_test"
	if _, err := admin.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE `" + name + "`"); admin.Close() })
	config.DBName = name
	db, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMySQLAlbumDerivedMigrationFromEmptyAndVideoSchema(t *testing.T) {
	for _, start := range []string{"empty", "0025", "0026-interrupted"} {
		t.Run(start, func(t *testing.T) {
			db := migrationTestDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			owner, id := uuid.NewString(), uuid.NewString()
			if start != "empty" {
				if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (name VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6))`); err != nil {
					t.Fatal(err)
				}
				entries, err := fs.ReadDir(migrationFiles, ".")
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if !isMigrationFile(entry.Name()) || entry.Name() > "0025_album_video_jobs.sql" {
						continue
					}
					contents, err := migrationFiles.ReadFile(entry.Name())
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(ctx, string(contents)); err != nil {
						t.Fatal(entry.Name(), err)
					}
					if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(name) VALUES(?)`, entry.Name()); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO album_storage_accounts(owner_id,quota_bytes,used_bytes,reserved_bytes) VALUES(?,1024,10,18)`, owner); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO album_uploads(upload_id,owner_id,request_id,manifest_digest,manifest_json,state,original_bytes,derived_budget,expires_at) VALUES(?,?,?,?,?,'originals_verified',10,18,?)`, id, owner, uuid.NewString(), strings.Repeat("a", 64), `{"migrationFixture":true}`, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				if start == "0026-interrupted" {
					for _, ddl := range []string{
						`ALTER TABLE album_uploads ADD COLUMN derived_budget_closed BOOLEAN NOT NULL DEFAULT FALSE, ADD KEY album_derived_pending(state,derived_budget_closed,updated_at,upload_id)`,
						`ALTER TABLE album_photo_writes ADD COLUMN retirement_started_at DATETIME(6) NULL`,
						`ALTER TABLE album_video_jobs ADD COLUMN retirement_started_at DATETIME(6) NULL`,
					} {
						if _, err := db.ExecContext(ctx, ddl); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			for range 2 {
				if err := Run(ctx, db); err != nil {
					t.Fatal("migration or repeat failed", err)
				}
			}
			var count int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name='0026_album_derived_finalizations.sql'`).Scan(&count); err != nil || count != 1 {
				t.Fatal("migration ledger duplicated", count, err)
			}
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='album_uploads' AND index_name='album_derived_pending'`).Scan(&count); err != nil || count != 4 {
				t.Fatal("pending scheduling index absent", count, err)
			}
			for _, table := range []string{"album_derived_finalizations", "album_derived_cleanups"} {
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
					t.Fatal("migration invented derived receipts", table, count, err)
				}
			}
			if start != "empty" {
				var used, reserved int64
				var closed bool
				if err := db.QueryRowContext(ctx, `SELECT used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&used, &reserved); err != nil || used != 10 || reserved != 18 {
					t.Fatal("upgrade changed existing storage balance", used, reserved, err)
				}
				if err := db.QueryRowContext(ctx, `SELECT derived_budget_closed FROM album_uploads WHERE upload_id=?`, id).Scan(&closed); err != nil || closed {
					t.Fatal("legacy upload incorrectly closed", closed, err)
				}
			}
		})
	}
}
