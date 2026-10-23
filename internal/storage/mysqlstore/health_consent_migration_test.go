package mysqlstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/platformhttpapi"
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
	// Published clients before guardian consent send this seven-scope batch.
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

func TestMySQLGuardianConsentScopeMigration(t *testing.T) {
	db := testutil.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := db.ExecContext(ctx, `ALTER TABLE privacy_consents
		DROP CHECK privacy_consents_scope_chk,
		ADD CONSTRAINT privacy_consents_scope_chk CHECK (scope IN
		('adult', 'age_14_plus', 'privacy_and_terms', 'lifetime_byok',
		'managed_subscription', 'free_managed_recognition', 'sensitive_health_ai'))`); err != nil {
		t.Fatal(err)
	}
	const migration = "0066_privacy_consent_scopes.sql"
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = ?`, migration); err != nil {
		t.Fatal(err)
	}
	principal := attestation.Principal{AppID: "health", KeyID: "guardian-consent", DeviceID: "00000000-0000-4000-8000-000000000083"}
	journal := principal
	journal.AppID = "journal"
	for _, value := range []attestation.Principal{principal, journal} {
		if err := mysqlstore.NewKeyRepository(db, value.AppID).Register(ctx, attestation.RegisteredKey{
			KeyID: value.KeyID, DeviceID: value.DeviceID, PublicKey: []byte("fixture"),
			Environment: "production", Receipt: []byte("fixture"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	repository := mysqlstore.NewPrivacyRepository(db, "health")
	service := privacy.NewService(repository, nil, nil, time.Now)
	consents := []privacy.Consent{
		{Scope: privacy.AdultScope, DocumentVersion: privacy.GeneralDocumentVersion},
		{Scope: privacy.Age14PlusScope, DocumentVersion: privacy.HealthGeneralDocumentVersion},
		{Scope: privacy.GuardianConsentScope, DocumentVersion: privacy.HealthCurrentDocumentVersion},
		{Scope: privacy.PrivacyTermsScope, DocumentVersion: privacy.HealthCurrentDocumentVersion, Granted: true},
		{Scope: privacy.LifetimeBYOKScope, DocumentVersion: privacy.AIDocumentVersion},
		{Scope: privacy.ManagedAIScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true},
		{Scope: privacy.FreeRecognitionScope, DocumentVersion: privacy.AIDocumentVersion},
		{Scope: privacy.SensitiveHealthScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true},
	}
	if err := repository.CheckConsentScopes(ctx, []string{privacy.GuardianConsentScope}); err == nil {
		t.Fatal("readiness accepted the incompatible deployed constraint")
	}
	_, err := service.RecordConsents(ctx, principal, consents)
	var constraintError *mysql.MySQLError
	if !errors.As(err, &constraintError) || constraintError.Number != 3819 {
		t.Fatalf("expected the production guardian scope rejection, got %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM privacy_consents`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected batch was partially saved: count=%d err=%v", count, err)
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, granted := range []bool{false, true} {
		consents[2].Granted = granted
		if _, err := service.RecordConsents(ctx, principal, consents); err != nil {
			t.Fatalf("guardian granted=%v failed after migration: %v", granted, err)
		}
		var stored bool
		if err := db.QueryRowContext(ctx, `SELECT granted FROM privacy_consents
			WHERE app_id='health' AND key_id=? AND scope='guardian_consent'`, principal.KeyID).Scan(&stored); err != nil || stored != granted {
			t.Fatalf("guardian state changed: got=%v want=%v err=%v", stored, granted, err)
		}
		allowed, err := service.HasRequiredConsents(ctx, principal, []string{privacy.HealthEligibilityScope, privacy.ManagedAIScope, privacy.SensitiveHealthScope})
		if err != nil || !allowed {
			t.Fatalf("complete consent batch denied: allowed=%v err=%v", allowed, err)
		}
	}
	consents[3].Granted = false
	if _, err := service.RecordConsents(ctx, principal, consents); err != nil {
		t.Fatal(err)
	}
	if allowed, err := service.HasRequiredConsents(ctx, principal, []string{privacy.HealthEligibilityScope}); err != nil || allowed {
		t.Fatalf("guardian consent replaced revoked general consent: allowed=%v err=%v", allowed, err)
	}
	journalService := privacy.NewService(mysqlstore.NewPrivacyRepository(db, "journal"), nil, nil, time.Now)
	if _, err := journalService.RecordConsents(ctx, journal, consents[2:3]); !errors.Is(err, privacy.ErrInvalidConsent) {
		t.Fatalf("Health guardian consent crossed the app boundary: %v", err)
	}
	illustration := []privacy.Consent{{Scope: privacy.JournalIllustrationScope, DocumentVersion: privacy.JournalIllustrationDocumentVersion, Granted: true}}
	if _, err := journalService.RecordConsents(ctx, journal, illustration); err != nil {
		t.Fatalf("existing Journal contract scope rejected: %v", err)
	}
	if _, err := service.RecordConsents(ctx, principal, illustration); !errors.Is(err, privacy.ErrInvalidConsent) {
		t.Fatalf("Journal illustration consent crossed the app boundary: %v", err)
	}
	if err := repository.RecordConsents(ctx, []privacy.Record{{KeyID: principal.KeyID, DeviceID: principal.DeviceID,
		Scope: "unknown_scope", DocumentVersion: privacy.AIDocumentVersion, RecordedAt: time.Now()}}); err == nil {
		t.Fatal("migration removed the database scope allowlist")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = ?`, migration); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Run(ctx, db); err != nil {
			t.Fatalf("migration checkpoint recovery failed: %v", err)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM privacy_consents`).Scan(&count); err != nil || count != 9 {
		t.Fatalf("migration replay changed existing consents: count=%d err=%v", count, err)
	}
}

func TestMySQLConsentContractScopesRemainWritable(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	document, err := platformhttpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	principal := attestation.RegisteredKey{KeyID: "contract-scopes", DeviceID: "00000000-0000-4000-8000-000000000084", PublicKey: []byte("fixture"), Environment: "production", Receipt: []byte("fixture")}
	if err := mysqlstore.NewKeyRepository(db, "health").Register(ctx, principal); err != nil {
		t.Fatal(err)
	}
	repository := mysqlstore.NewPrivacyRepository(db, "health")
	var scopes []string
	for _, value := range document.Components.Schemas["PrivacyConsent"].Value.Properties["scope"].Value.Enum {
		scope := value.(string)
		scopes = append(scopes, scope)
		for _, granted := range []bool{false, true} {
			if err := repository.RecordConsents(ctx, []privacy.Record{{KeyID: principal.KeyID, DeviceID: principal.DeviceID, Scope: scope, DocumentVersion: "contract-fixture", Granted: granted, RecordedAt: time.Now()}}); err != nil {
				t.Fatalf("contract scope %s granted=%v cannot be stored: %v", scope, granted, err)
			}
		}
	}
	if len(scopes) == 0 {
		t.Fatal("consent contract enum is empty")
	}
	if err := repository.CheckConsentScopes(ctx, scopes); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE privacy_consents ALTER CHECK privacy_consents_scope_chk NOT ENFORCED`); err != nil {
		t.Fatal(err)
	}
	if err := repository.CheckConsentScopes(ctx, scopes); err == nil {
		t.Fatal("readiness accepted a disabled database constraint")
	}
}
