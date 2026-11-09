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
	return run(ctx, database, migrationLockName, false)
}

// RunAlbumDevelopmentMetadata applies only the reviewed Albums development extensions.
func RunAlbumDevelopmentMetadata(ctx context.Context, database *sql.DB) error {
	return run(ctx, database, migrationLockName, true)
}

func albumDevelopmentMigration(name string) bool {
	switch name {
	case "0073_album_development_subscription_products.sql", "0074_album_system_copy_locators.sql",
		"0075_album_content_corrections.sql", "0076_album_asset_favorites.sql",
		"0077_album_collections.sql", "0078_album_asset_deletion_ledger.sql", "0080_album_photo_multipart.sql", "0081_album_photo_completion_fence.sql", "0082_album_retention_cleanup_schedule.sql", "0083_album_record_erasure_schedule.sql":
		return true
	default:
		return false
	}
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
	return run(ctx, database, fmt.Sprintf("tellyouwhat_test_migrations_%x", digest[:16]), false)
}

func run(ctx context.Context, database *sql.DB, lockName string, albumMetadataOnly bool) error {
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
	if albumMetadataOnly {
		var base int
		if err := connection.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name='0064_album_identity_retirement.sql'`).Scan(&base); err != nil {
			return fmt.Errorf("inspect Albums development migration base: %w", err)
		}
		if base != 1 {
			return fmt.Errorf("Albums development migration requires the existing identity-retirement schema")
		}
	}
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
		if applied[name] || (albumMetadataOnly && !albumDevelopmentMigration(name)) {
			continue
		}
		sourceName := name
		// Joined Health and Journal histories must never narrow an existing consent allowlist.
		switch name {
		case "0066_privacy_consent_scopes.sql", "0067_journal_illustration_consent_scope.sql", "0070_journal_read_aloud_consent.sql":
			sourceName = "0072_health_guardian_consent_scope.sql"
		}
		contents, err := migrationFiles.ReadFile(sourceName)
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
