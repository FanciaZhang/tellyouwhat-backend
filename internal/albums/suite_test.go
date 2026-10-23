package albums

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

// Legacy tests share only their process's parent schema; each shard owns its cleanup.
func TestMain(m *testing.M) {
	raw := os.Getenv("MYSQL_TEST_ADMIN_DSN")
	if raw == "" {
		os.Exit(m.Run())
	}
	config, err := mysql.ParseDSN(raw)
	if err != nil || !strings.HasSuffix(config.DBName, "_test") {
		fmt.Fprintln(os.Stderr, "requires an explicitly designated test database")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	admin, err := mysqlstore.Open(ctx, raw)
	if err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "isolated test suite database unavailable")
		os.Exit(1)
	}
	name := "albums_suite_" + strings.ReplaceAll(uuid.NewString(), "-", "") + "_test"
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		cancel()
		admin.Close()
		fmt.Fprintln(os.Stderr, "isolated suite creation failed")
		os.Exit(1)
	}
	config.DBName = name
	_ = os.Setenv("MYSQL_TEST_DSN", config.FormatDSN())
	cancel()
	code := m.Run()
	if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
		fmt.Fprintln(os.Stderr, "isolated suite cleanup failed")
		code = 1
	}
	admin.Close()
	os.Exit(code)
}
