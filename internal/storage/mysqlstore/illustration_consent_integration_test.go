package mysqlstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
	"github.com/tellyouwhat/backend/migrations"
)

func TestMySQLIllustrationConsentUpgradePreservesExistingGrantsAndReplays(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	owner := attestation.RegisteredKey{KeyID: uuid.NewString(), DeviceID: uuid.NewString(),
		PublicKey: []byte("synthetic"), Environment: "production", Receipt: []byte("synthetic")}
	if err := mysqlstore.NewKeyRepository(db, "journal").Register(ctx, owner); err != nil {
		t.Fatal(err)
	}
	repository := mysqlstore.NewPrivacyRepository(db, "journal")
	now := time.Now().UTC().Truncate(time.Microsecond)
	record := privacy.Record{KeyID: owner.KeyID, DeviceID: owner.DeviceID,
		Scope: privacy.ManagedAIScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true, RecordedAt: now}
	if err := repository.RecordConsents(ctx, []privacy.Record{record}); err != nil {
		t.Fatal(err)
	}
	// Recreate the deployed schema in this test's private database, then upgrade it.
	if _, err := db.ExecContext(ctx, `ALTER TABLE privacy_consents DROP CHECK privacy_consents_scope_chk,
		ADD CONSTRAINT privacy_consents_scope_chk CHECK (scope IN (
		'adult','age_14_plus','privacy_and_terms','lifetime_byok','managed_subscription',
		'free_managed_recognition','sensitive_health_ai'))`); err != nil {
		t.Fatal(err)
	}
	const migration = "0067_journal_illustration_consent_scope.sql"
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name=?`, migration); err != nil {
		t.Fatal(err)
	}
	image := record
	image.Scope, image.DocumentVersion = privacy.JournalIllustrationScope, privacy.JournalIllustrationDocumentVersion
	if err := repository.RecordConsents(ctx, []privacy.Record{image}); err == nil {
		t.Fatal("pre-upgrade schema unexpectedly accepted illustration consent")
	}
	for range 2 {
		if err := migrations.Run(ctx, db); err != nil {
			t.Fatal(err)
		}
		if err := repository.RecordConsents(ctx, []privacy.Record{image}); err != nil {
			t.Fatal("illustration consent could not be persisted", err)
		}
		for _, expected := range []privacy.Record{record, image} {
			granted, err := repository.HasGrantedConsents(ctx, owner.KeyID,
				[]privacy.Consent{{Scope: expected.Scope, DocumentVersion: expected.DocumentVersion, Granted: true}})
			if err != nil || !granted {
				t.Fatalf("grant not preserved: %s: %v", expected.Scope, err)
			}
		}
		// Simulate loss of the migration checkpoint after its ALTER committed.
		if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name=?`, migration); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	image.Granted = false
	if err := repository.RecordConsents(ctx, []privacy.Record{image}); err != nil {
		t.Fatal(err)
	}
	granted, err := repository.HasGrantedConsents(ctx, owner.KeyID,
		[]privacy.Consent{{Scope: image.Scope, DocumentVersion: image.DocumentVersion, Granted: true}})
	if err != nil || granted {
		t.Fatal("withdrawal was not persisted", err)
	}
	image.Scope = "unknown_scope"
	if err := repository.RecordConsents(ctx, []privacy.Record{image}); err == nil {
		t.Fatal("database stopped rejecting unknown scopes")
	}
}
