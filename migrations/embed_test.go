package migrations

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedMigrationsStartFromCleanMultiAppSchema(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(migrationFiles, ".")
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"0001_initial.sql"}
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

func TestInitialSchemaUsesTwoRolesAndPasswordlessRecoveryInvitations(t *testing.T) {
	t.Parallel()
	contents, err := migrationFiles.ReadFile("0001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(contents)
	for _, required := range []string{
		"CHECK (role IN ('admin', 'operator'))",
		"CHECK (status IN ('active', 'disabled'))",
		"CREATE TABLE IF NOT EXISTS admin_control_state",
		"CREATE TABLE IF NOT EXISTS admin_user_apps",
		"CREATE TABLE IF NOT EXISTS admin_invitations",
		"CREATE TABLE IF NOT EXISTS admin_invitation_apps",
		"CREATE TABLE IF NOT EXISTS admin_audit_events",
		"session_version BIGINT UNSIGNED",
		"'health.premium.subscription.monthly'",
		"'free_managed_recognition'",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("initial schema is missing %q", required)
		}
	}
	for _, obsolete := range []string{
		"admin_recovery_codes", "admin_app_roles", "CHECK (role IN ('owner'))", "password_hash", "totp_secret",
	} {
		if strings.Contains(schema, obsolete) {
			t.Fatalf("initial schema retained obsolete authentication surface %q", obsolete)
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
