package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

//go:embed *.sql
var migrationFiles embed.FS

const migrationLockName = "tellyouwhat_platform_schema_migrations"

func Run(ctx context.Context, database *sql.DB) error {
	return run(ctx, database, migrationLockName)
}

// RunIsolatedTest lets independent test databases migrate without sharing the production lock.
func RunIsolatedTest(ctx context.Context, database *sql.DB) error {
	var name string
	if err := database.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&name); err != nil {
		return err
	}
	if !strings.HasSuffix(name, "_test") {
		return fmt.Errorf("isolated migration requires a test database")
	}
	digest := sha256.Sum256([]byte(name))
	return run(ctx, database, fmt.Sprintf("tellyouwhat_test_migrations_%x", digest[:16]))
}

func run(ctx context.Context, database *sql.DB, lockName string) error {
	connection, err := database.Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	var acquired sql.NullInt64
	if err := connection.QueryRowContext(ctx, `SELECT GET_LOCK(?, 30)`, lockName).Scan(&acquired); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return fmt.Errorf("lock migrations: timeout")
	}
	defer func() {
		_, _ = connection.ExecContext(context.Background(), `SELECT RELEASE_LOCK(?)`, lockName)
	}()
	var unscopedTables int
	if err := connection.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM information_schema.tables AS existing
        WHERE existing.table_schema = DATABASE()
          AND existing.table_name IN ('apps', 'app_attest_keys', 'managed_entitlements')
          AND NOT EXISTS (
              SELECT 1 FROM information_schema.columns AS columns_in_table
              WHERE columns_in_table.table_schema = existing.table_schema
                AND columns_in_table.table_name = existing.table_name
                AND columns_in_table.column_name = 'app_id'
          )`).Scan(&unscopedTables); err != nil {
		return fmt.Errorf("inspect database app isolation: %w", err)
	}
	if unscopedTables > 0 {
		return fmt.Errorf("refusing to migrate a legacy single-app schema: use a separate database for the shared backend")
	}
	if _, err := connection.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS schema_migrations (
            name VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
            applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
        ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFiles, ".")
	if err != nil {
		return err
	}
	rows, err := connection.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("inspect applied migrations: %w", err)
	}
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		applied[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !isMigrationFile(entry.Name()) {
			continue
		}
		name := entry.Name()
		if applied[name] {
			continue
		}
		contents, err := migrationFiles.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err = connection.ExecContext(ctx, string(contents)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err = connection.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}
	return nil
}

func isMigrationFile(name string) bool {
	return !strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".sql")
}
