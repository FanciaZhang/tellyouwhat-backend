package testutil

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

// MySQL gives each test its own database so global policy and accounting tests
// cannot change the databases used by concurrently running packages.
func MySQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_ADMIN_DSN is required for isolated database tests")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.DBName, "_test") {
		t.Fatal("requires an explicitly designated test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := mysqlstore.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "ops_" + strings.ReplaceAll(uuid.NewString(), "-", "") + "_test"
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE `" + name + "`"); admin.Close() })
	cfg.DBName = name
	db, err := mysqlstore.Open(ctx, cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}
