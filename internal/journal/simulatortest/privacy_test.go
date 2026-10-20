package simulatortest

import (
	"context"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/testutil"
	"github.com/tellyouwhat/backend/migrations"
)

func TestSimulatorConsentsStaySeparateFromAppleKeysAndRevokeDurably(t *testing.T) {
	db := testutil.MySQL(t)
	f := newFixture(t)
	ctx := context.Background()
	normal := privacy.NewMemoryRepository()
	repo := PrivacyRepository{Repository: normal, DB: db, Simulator: f.s}
	if err := repo.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	service := privacy.NewService(repo, nil, nil, func() time.Time { return f.now })
	p := attestation.Principal{AppID: "journal", KeyID: f.r.Credentials[0].KeyID, DeviceID: f.r.Credentials[0].KeyID}
	consent := privacy.Consent{Scope: privacy.JournalReadAloudScope, DocumentVersion: privacy.JournalReadAloudDocumentVersion, Granted: true}
	if ok, err := service.HasRequiredConsents(ctx, p, []string{consent.Scope}); err != nil || ok {
		t.Fatal("new identity inherited consent", ok, err)
	}
	if _, err := service.RecordConsents(ctx, p, []privacy.Consent{consent}); err != nil {
		t.Fatal(err)
	}
	if ok, err := service.HasRequiredConsents(ctx, p, []string{consent.Scope}); err != nil || !ok {
		t.Fatal("real consent missing", ok, err)
	}
	// Rebuilding the service must read the durable database rather than a fixture flag.
	service = privacy.NewService(PrivacyRepository{Repository: normal, DB: db, Simulator: f.s}, nil, nil, func() time.Time { return f.now })
	if ok, err := service.HasRequiredConsents(ctx, p, []string{consent.Scope}); err != nil || !ok {
		t.Fatal("consent lost after restart", ok, err)
	}
	consent.Granted = false
	if _, err := service.RecordConsents(ctx, p, []privacy.Consent{consent}); err != nil {
		t.Fatal(err)
	}
	if ok, err := service.HasRequiredConsents(ctx, p, []string{consent.Scope}); err != nil || ok {
		t.Fatal("revoked consent remained active", ok, err)
	}
	for _, table := range []string{"app_attest_keys", "managed_entitlements", "privacy_consents"} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE key_id=?", p.KeyID).Scan(&count); err != nil || count != 0 {
			t.Fatal("test identity contaminated Apple records", table, count, err)
		}
	}
	consent.Scope = privacy.ManagedAIScope
	consent.DocumentVersion = privacy.AIDocumentVersion
	if _, err := service.RecordConsents(ctx, p, []privacy.Consent{consent}); err != privacy.ErrInvalidConsent {
		t.Fatal("test identity authorized another service", err)
	}
	if _, err := db.Exec(`INSERT INTO journal_simulator_read_aloud_consents VALUES (?,'managed_subscription','2026-08-24',true,?,?)`, p.KeyID, f.now, f.now.Add(time.Hour)); err == nil {
		t.Fatal("database allowed another scope")
	}
	consent.Scope = privacy.JournalReadAloudScope
	consent.DocumentVersion = privacy.JournalReadAloudDocumentVersion
	consent.Granted = true
	p.KeyID = "normal-key"
	p.DeviceID = "normal-device"
	if _, err := service.RecordConsents(ctx, p, []privacy.Consent{consent}); err != nil {
		t.Fatal(err)
	}
	if ok, err := normal.HasGrantedConsents(ctx, p.KeyID, []privacy.Consent{consent}); err != nil || !ok {
		t.Fatal("normal consent repository was bypassed", err)
	}
	f.r.Credentials[0].Revoked = true
	f.write(t)
	if _, err := repo.HasGrantedConsents(ctx, f.r.Credentials[0].KeyID, []privacy.Consent{consent}); err != privacy.ErrConsentUnavailable {
		t.Fatal("revocation did not affect existing consent", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name='0071_journal_simulator_read_aloud_consents.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal("DDL could not be safely replayed", err)
	}
}
