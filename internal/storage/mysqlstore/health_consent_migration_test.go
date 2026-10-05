package mysqlstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
	"github.com/tellyouwhat/backend/migrations"
)

func TestMySQLHealthConsentScopeMigration(t *testing.T) {
	db := testutil.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Recreate the deployed constraint, including on a freshly migrated database.
	var constraint string
	if err := db.QueryRowContext(ctx, `SELECT CONSTRAINT_NAME FROM information_schema.TABLE_CONSTRAINTS
		WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'privacy_consents'
		AND CONSTRAINT_TYPE = 'CHECK'`).Scan(&constraint); err != nil {
		t.Fatal(err)
	}
	if constraint != "privacy_consents_chk_1" && constraint != "privacy_consents_scope_chk" {
		t.Fatalf("unexpected consent constraint: %s", constraint)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE privacy_consents DROP CHECK `+constraint+`,
		ADD CONSTRAINT privacy_consents_chk_1 CHECK (scope IN
		('adult', 'privacy_and_terms', 'lifetime_byok', 'managed_subscription',
		'free_managed_recognition', 'sensitive_health_ai'))`); err != nil {
		t.Fatal(err)
	}
	const migration = "0014_health_age_consent_scope.sql"
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = ?`, migration); err != nil {
		t.Fatal(err)
	}

	legacy := attestation.Principal{AppID: "health", KeyID: "legacy-consent", DeviceID: "00000000-0000-4000-8000-000000000081"}
	current := attestation.Principal{AppID: "health", KeyID: "current-consent", DeviceID: "00000000-0000-4000-8000-000000000082"}
	journal := legacy
	journal.AppID = "journal"
	for _, principal := range []attestation.Principal{legacy, current, journal} {
		if err := mysqlstore.NewKeyRepository(db, principal.AppID).Register(ctx, attestation.RegisteredKey{
			KeyID: principal.KeyID, DeviceID: principal.DeviceID, PublicKey: []byte("fixture"),
			Environment: "production", Receipt: []byte("fixture"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	repository := mysqlstore.NewPrivacyRepository(db, "health")
	service := privacy.NewService(repository, nil, nil, time.Now)
	journalService := privacy.NewService(mysqlstore.NewPrivacyRepository(db, "journal"), nil, nil, time.Now)
	legacyConsents := []privacy.Consent{
		{Scope: privacy.AdultScope, DocumentVersion: privacy.GeneralDocumentVersion, Granted: true},
		{Scope: privacy.PrivacyTermsScope, DocumentVersion: privacy.GeneralDocumentVersion, Granted: true},
		{Scope: privacy.SensitiveHealthScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true},
	}
	for _, principal := range []attestation.Principal{legacy, journal} {
		target := service
		if principal.AppID == "journal" {
			target = journalService
		}
		if _, err := target.RecordConsents(ctx, principal, legacyConsents); err != nil {
			t.Fatal(err)
		}
	}
	// This is the complete seven-scope batch sent by current Health clients.
	currentConsents := []privacy.Consent{
		{Scope: privacy.AdultScope, DocumentVersion: privacy.GeneralDocumentVersion, Granted: false},
		{Scope: privacy.Age14PlusScope, DocumentVersion: privacy.HealthGeneralDocumentVersion, Granted: true},
		{Scope: privacy.PrivacyTermsScope, DocumentVersion: privacy.HealthGeneralDocumentVersion, Granted: true},
		{Scope: privacy.LifetimeBYOKScope, DocumentVersion: privacy.AIDocumentVersion, Granted: false},
		{Scope: privacy.ManagedAIScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true},
		{Scope: privacy.FreeRecognitionScope, DocumentVersion: privacy.AIDocumentVersion, Granted: false},
		{Scope: privacy.SensitiveHealthScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true},
	}
	_, err := service.RecordConsents(ctx, current, currentConsents)
	var constraintError *mysql.MySQLError
	if !errors.As(err, &constraintError) || constraintError.Number != 3819 {
		t.Fatalf("expected the deployed CHECK constraint rejection, got %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM privacy_consents WHERE app_id = 'health' AND key_id = ?`, current.KeyID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed consent batch was not atomic: count=%d err=%v", count, err)
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RecordConsents(ctx, current, currentConsents); err != nil {
		t.Fatalf("current Health consent batch failed after upgrade: %v", err)
	}
	for _, principal := range []attestation.Principal{legacy, current} {
		granted, err := service.HasRequiredConsents(ctx, principal, []string{privacy.HealthEligibilityScope, privacy.SensitiveHealthScope})
		if err != nil || !granted {
			t.Fatalf("Health eligibility lost for %s: granted=%v err=%v", principal.KeyID, granted, err)
		}
	}
	if _, err := service.RecordConsents(ctx, legacy, legacyConsents); err != nil {
		t.Fatalf("old Health client cannot write after upgrade: %v", err)
	}
	if granted, err := journalService.HasRequiredConsents(ctx, journal, []string{privacy.AdultScope, privacy.PrivacyTermsScope, privacy.SensitiveHealthScope}); err != nil || !granted {
		t.Fatalf("Journal legacy consent changed: granted=%v err=%v", granted, err)
	}
	if _, err := journalService.RecordConsents(ctx, journal, currentConsents); !errors.Is(err, privacy.ErrInvalidConsent) {
		t.Fatalf("Health age consent must remain unavailable to Journal: %v", err)
	}
	if err := repository.RecordConsents(ctx, []privacy.Record{{KeyID: current.KeyID, DeviceID: current.DeviceID,
		Scope: "unknown_scope", DocumentVersion: privacy.AIDocumentVersion, Granted: true, RecordedAt: time.Now()}}); err == nil {
		t.Fatal("migration removed the database scope allowlist")
	}
	if _, err := service.RecordConsents(ctx, current, []privacy.Consent{{Scope: privacy.Age14PlusScope,
		DocumentVersion: privacy.HealthGeneralDocumentVersion, Granted: false}}); err != nil {
		t.Fatal(err)
	}
	if granted, err := service.HasRequiredConsents(ctx, current, []string{privacy.HealthEligibilityScope}); err != nil || granted {
		t.Fatalf("revoked age consent still authorized Health: granted=%v err=%v", granted, err)
	}
	// Recover a completed DDL whose migration checkpoint was not committed.
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = ?`, migration); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Run(ctx, db); err != nil {
			t.Fatalf("migration replay failed: %v", err)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM privacy_consents`).Scan(&count); err != nil || count != 13 {
		t.Fatalf("migration replay changed existing consents: count=%d err=%v", count, err)
	}
}
