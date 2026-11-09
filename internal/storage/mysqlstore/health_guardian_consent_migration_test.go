package mysqlstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
	"github.com/tellyouwhat/backend/migrations"
)

func TestMySQLHealthGuardianConsentUpgradePreservesCurrentBatch(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	owner := attestation.RegisteredKey{KeyID: uuid.NewString(), DeviceID: uuid.NewString(), PublicKey: []byte("synthetic"), Environment: "production", Receipt: []byte("synthetic")}
	if err := mysqlstore.NewKeyRepository(db, "health").Register(ctx, owner); err != nil {
		t.Fatal(err)
	}
	principal := attestation.Principal{AppID: "health", KeyID: owner.KeyID, DeviceID: owner.DeviceID}
	repo := mysqlstore.NewPrivacyRepository(db, "health")
	service := privacy.NewService(repo, nil, nil, time.Now)
	batch := []privacy.Consent{
		{Scope: privacy.AdultScope, DocumentVersion: privacy.GeneralDocumentVersion, Granted: false},
		{Scope: privacy.Age14PlusScope, DocumentVersion: privacy.HealthGeneralDocumentVersion, Granted: false},
		{Scope: privacy.GuardianConsentScope, DocumentVersion: privacy.HealthCurrentDocumentVersion, Granted: false},
		{Scope: privacy.PrivacyTermsScope, DocumentVersion: privacy.HealthCurrentDocumentVersion, Granted: true},
		{Scope: privacy.LifetimeBYOKScope, DocumentVersion: privacy.AIDocumentVersion, Granted: false},
		{Scope: privacy.ManagedAIScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true},
		{Scope: privacy.FreeRecognitionScope, DocumentVersion: privacy.AIDocumentVersion, Granted: false},
		{Scope: privacy.SensitiveHealthScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true},
	}
	// Reproduce the deployed CHECK constraint rejecting even a false guardian decision.
	if _, err := db.ExecContext(ctx, `ALTER TABLE privacy_consents DROP CHECK privacy_consents_scope_chk,
		ADD CONSTRAINT privacy_consents_scope_chk CHECK (scope IN ('adult','age_14_plus','privacy_and_terms',
		'lifetime_byok','managed_subscription','free_managed_recognition','sensitive_health_ai','journal_illustration','journal_read_aloud'))`); err != nil {
		t.Fatal(err)
	}
	_, err := service.RecordConsents(ctx, principal, batch)
	var constraintError *mysql.MySQLError
	if !errors.As(err, &constraintError) || constraintError.Number != 3819 {
		t.Fatalf("expected deployed CHECK rejection, got %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM privacy_consents WHERE key_id=?`, owner.KeyID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed batch must roll back: count=%d err=%v", count, err)
	}
	const migration = "0072_health_guardian_consent_scope.sql"
	for range 2 {
		if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name=?`, migration); err != nil {
			t.Fatal(err)
		}
		if err := migrations.Run(ctx, db); err != nil {
			t.Fatal(err)
		}
		if _, err := service.RecordConsents(ctx, principal, batch); err != nil {
			t.Fatalf("complete current Health batch must synchronize: %v", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM privacy_consents WHERE key_id=?`, owner.KeyID).Scan(&count); err != nil || count != 8 {
			t.Fatalf("migration replay changed the consent batch: count=%d err=%v", count, err)
		}
	}
	if granted, err := service.HasRequiredConsents(ctx, principal, []string{privacy.HealthEligibilityScope, privacy.ManagedAIScope, privacy.SensitiveHealthScope}); err != nil || !granted {
		t.Fatalf("current Health access must remain authorized: granted=%v err=%v", granted, err)
	}
	for _, granted := range []bool{true, false} {
		guardian := batch[2]
		guardian.Granted = granted
		if _, err := service.RecordConsents(ctx, principal, []privacy.Consent{guardian}); err != nil {
			t.Fatal(err)
		}
		actual, err := repo.HasGrantedConsents(ctx, owner.KeyID, []privacy.Consent{guardian})
		if err != nil || actual != granted {
			t.Fatalf("guardian decision not retained: granted=%v err=%v", actual, err)
		}
	}
	if err := repo.RecordConsents(ctx, []privacy.Record{{KeyID: owner.KeyID, DeviceID: owner.DeviceID,
		Scope: "unknown_scope", DocumentVersion: privacy.AIDocumentVersion, RecordedAt: time.Now()}}); err == nil {
		t.Fatal("unknown scopes must remain rejected")
	}
}
