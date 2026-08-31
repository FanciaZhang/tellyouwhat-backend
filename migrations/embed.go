package migrations

import (
	"context"
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
	connection, err := database.Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	var acquired sql.NullInt64
	if err := connection.QueryRowContext(ctx, `SELECT GET_LOCK(?, 30)`, migrationLockName).Scan(&acquired); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return fmt.Errorf("lock migrations: timeout")
	}
	defer func() {
		_, _ = connection.ExecContext(context.Background(), `SELECT RELEASE_LOCK(?)`, migrationLockName)
	}()
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
	for _, entry := range entries {
		if entry.IsDir() || !isMigrationFile(entry.Name()) {
			continue
		}
		name := entry.Name()
		contents, err := migrationFiles.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		var applied int
		err = connection.QueryRowContext(ctx, `
            SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&applied)
		if err != nil {
			return fmt.Errorf("inspect migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
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
