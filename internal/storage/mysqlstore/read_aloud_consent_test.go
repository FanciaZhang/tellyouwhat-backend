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

func TestMySQLReadAloudConsentUpgradePreservesGrantsAndRevokes(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	owner := attestation.RegisteredKey{KeyID: uuid.NewString(), DeviceID: uuid.NewString(), PublicKey: []byte("synthetic"), Environment: "production", Receipt: []byte("synthetic")}
	if err := mysqlstore.NewKeyRepository(db, "journal").Register(ctx, owner); err != nil {
		t.Fatal(err)
	}
	repo := mysqlstore.NewPrivacyRepository(db, "journal")
	now := time.Now().UTC().Truncate(time.Microsecond)
	before := []privacy.Record{
		{KeyID: owner.KeyID, DeviceID: owner.DeviceID, Scope: privacy.ManagedAIScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true, RecordedAt: now},
		{KeyID: owner.KeyID, DeviceID: owner.DeviceID, Scope: privacy.JournalIllustrationScope, DocumentVersion: privacy.JournalIllustrationDocumentVersion, Granted: true, RecordedAt: now},
	}
	if err := repo.RecordConsents(ctx, before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE privacy_consents DROP CHECK privacy_consents_scope_chk, ADD CONSTRAINT privacy_consents_scope_chk CHECK (scope IN ('adult','age_14_plus','privacy_and_terms','lifetime_byok','managed_subscription','free_managed_recognition','sensitive_health_ai','journal_illustration'))"); err != nil {
		t.Fatal(err)
	}
	const migration = "0070_journal_read_aloud_consent.sql"
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE name=?", migration); err != nil {
		t.Fatal(err)
	}
	tts := before[0]
	tts.Scope = privacy.JournalReadAloudScope
	tts.DocumentVersion = privacy.JournalReadAloudDocumentVersion
	if err := repo.RecordConsents(ctx, []privacy.Record{tts}); err == nil {
		t.Fatal("old constraint unexpectedly accepted TTS")
	}
	for range 2 {
		if err := migrations.Run(ctx, db); err != nil {
			t.Fatal(err)
		}
		if err := repo.RecordConsents(ctx, []privacy.Record{tts}); err != nil {
			t.Fatal(err)
		}
		for _, record := range append(before, tts) {
			ok, err := repo.HasGrantedConsents(ctx, owner.KeyID, []privacy.Consent{{Scope: record.Scope, DocumentVersion: record.DocumentVersion, Granted: true}})
			if err != nil || !ok {
				t.Fatal("consent was not preserved", record.Scope, err)
			}
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE name=?", migration); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	tts.Granted = false
	if err := repo.RecordConsents(ctx, []privacy.Record{tts}); err != nil {
		t.Fatal(err)
	}
	ok, err := repo.HasGrantedConsents(ctx, owner.KeyID, []privacy.Consent{{Scope: tts.Scope, DocumentVersion: tts.DocumentVersion, Granted: true}})
	if err != nil || ok {
		t.Fatal("withdrawal was not durable", err)
	}
	tts.Scope = "unknown_scope"
	if err := repo.RecordConsents(ctx, []privacy.Record{tts}); err == nil {
		t.Fatal("unknown scope accepted")
	}
}
