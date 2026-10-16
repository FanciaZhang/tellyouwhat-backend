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
	expected := []string{"0001_initial.sql", "0002_voice_subscription_anchor.sql", "0003_privacy_deletion_receipts.sql", "0004_ai_cost_control.sql", "0005_health_ai_config.sql", "0006_health_ai_rollout.sql", "0007_platform_operations.sql", "0008_operations_insights.sql", "0009_operations_automation.sql", "0010_prompt_configuration.sql", "0011_prompt_evaluations.sql", "0012_offer_delivery.sql", "0013_subscription_environment_bindings.sql", "0014_health_age_consent_scope.sql", "0015_album_uploads.sql", "0015_birthday_app.sql", "0015_journal_illustration_prompt_baseline.sql", "0016_album_login_challenges.sql", "0017_album_accounts.sql", "0018_album_sessions.sql", "0019_album_multipart_intents.sql", "0020_album_multipart_completions.sql", "0021_album_reconciliation_queue.sql", "0022_album_asset_versions.sql", "0023_album_photo_writes.sql", "0024_album_photo_jobs.sql", "0025_album_video_jobs.sql", "0026_album_derived_finalizations.sql", "0027_album_live_pairs.sql", "0028_album_cloud_subscriptions.sql", "0029_album_subscription_reconciliation.sql", "0030_album_share_links.sql", "0031_album_retention_reminders.sql", "0032_album_push_registrations.sql", "0033_album_retention_deliveries.sql", "0034_album_asset_lifecycle.sql"}
	expected = append(expected, "0035_album_object_purges.sql", "0036_album_purge_settlement.sql", "0037_album_retention_purge_gate.sql", "0038_album_original_seals.sql", "0039_album_initialization_reconciliation.sql", "0040_album_apple_revocation.sql")
	expected = append(expected, "0041_album_account_deletions.sql", "0042_album_deletion_apple_jobs.sql")
	expected = append(expected, "0043_album_deletion_storage_fence.sql", "0044_album_deletion_video_jobs.sql", "0045_album_deletion_original_checks.sql", "0046_album_live_pair_copy_receipts.sql", "0047_album_deletion_live_pair_checks.sql", "0048_album_deletion_photo_checks.sql", "0049_album_direct_upload_evidence.sql", "0050_album_live_pair_multiparts.sql", "0051_album_live_multipart_reconciliation.sql", "0052_album_deletion_multipart_checks.sql", "0053_album_deletion_media_quiescence.sql", "0054_album_account_object_purges.sql", "0055_album_account_media_settlement.sql", "0056_album_account_access_erasure.sql", "0057_album_account_media_erasure_schedule.sql", "0058_album_storekit_account_token.sql", "0059_album_deletion_subscription_jobs.sql", "0060_album_deletion_ledger.sql", "0061_album_recovery_quarantine.sql", "0062_album_subscription_completion_receipt.sql")
	expected = append(expected, "0063_album_account_record_erasure.sql")
	expected = append(expected, "0064_album_identity_retirement.sql", "0065_journal_illustration_jobs.sql")
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
