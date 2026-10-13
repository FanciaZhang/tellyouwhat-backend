package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/albums"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

func TestWorkerRecoveryAdmissionBeforeProvider(t *testing.T) {
	raw := os.Getenv("MYSQL_TEST_ADMIN_DSN")
	if raw == "" {
		t.Skip("set MYSQL_TEST_ADMIN_DSN for isolated worker integration")
	}
	cfg, err := mysql.ParseDSN(raw)
	if err != nil || !strings.HasSuffix(cfg.DBName, "_test") {
		t.Fatal("refusing non-test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := mysqlstore.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "albums_worker_" + strings.ReplaceAll(uuid.NewString(), "-", "") + "_test"
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE `"+name+"`"); err != nil {
			t.Error("isolated database cleanup failed")
		}
	}()
	cfg.DBName = name
	db, err := mysqlstore.Open(ctx, cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Deliberately invalid COS configuration proves the recovery guard runs
	// before provider construction. No provider request is possible here.
	config := albums.WorkerConfig{DatabaseDSN: cfg.FormatDSN()}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, step := range []struct{ name, sql, want string }{
		{"fresh", "", "object storage configuration"},
		{"quarantined", `UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=UTC_TIMESTAMP(6)`, "album recovery state unavailable or quarantined"},
		{"missing row", `DELETE FROM album_recovery_state`, "album recovery state unavailable or quarantined"},
		{"missing table", `DROP TABLE album_recovery_state`, "album recovery state unavailable or quarantined"},
	} {
		t.Run(step.name, func(t *testing.T) {
			if step.sql != "" {
				if _, err := db.ExecContext(ctx, step.sql); err != nil {
					t.Fatal(err)
				}
			}
			if err := run(ctx, config, logger); err == nil || err.Error() != step.want {
				t.Fatalf("unexpected admission result: %v", err)
			}
		})
	}
}
