package migrations

import (
	"io/fs"
	"testing"
)

func TestEmbeddedMigrationsAreOrderedAndIncludeRetentionCleanup(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(migrationFiles, ".")
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"0001_initial.sql", "0002_retention.sql", "0003_privacy.sql", "0004_admin_portal.sql", "0005_admin_operations.sql", "0006_offer_redemptions.sql"}
	if len(entries) != len(expected) {
		t.Fatalf("unexpected migration order: %+v", entries)
	}
	for index, entry := range entries {
		if entry.Name() != expected[index] {
			t.Fatalf("unexpected migration order: %+v", entries)
		}
		contents, err := migrationFiles.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if len(contents) == 0 {
			t.Fatalf("migration is empty: %s", entry.Name())
		}
	}
}

func TestMigrationFileFilterRejectsHiddenMetadata(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"._0001_initial.sql", ".0001_initial.sql", "README.md"} {
		if isMigrationFile(name) {
			t.Fatalf("unexpected migration file: %s", name)
		}
	}
	if !isMigrationFile("0001_initial.sql") {
		t.Fatal("numbered SQL migration was rejected")
	}
}
