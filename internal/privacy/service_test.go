package privacy

import (
	"context"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
)

func TestRecordConsentsRequiresKnownVersionedScopes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	repository := NewMemoryRepository()
	service := NewService(repository, noopObjectCleaner{}, nil, func() time.Time { return now })
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}

	recordedAt, err := service.RecordConsents(context.Background(), principal, []Consent{
		{Scope: ManagedAIScope, DocumentVersion: AIDocumentVersion, Granted: true},
		{Scope: SensitiveHealthScope, DocumentVersion: AIDocumentVersion, Granted: false},
	})
	if err != nil || !recordedAt.Equal(now) || len(repository.records) != 2 {
		t.Fatalf("record valid consents: at=%v records=%d err=%v", recordedAt, len(repository.records), err)
	}
	if _, err := service.RecordConsents(context.Background(), principal, []Consent{
		{Scope: ManagedAIScope, DocumentVersion: "stale", Granted: true},
	}); err != ErrInvalidConsent {
		t.Fatalf("expected invalid consent version, got %v", err)
	}
}

func TestRequiredConsentsMustAllBeExplicitlyGranted(t *testing.T) {
	t.Parallel()
	repository := NewMemoryRepository()
	service := NewService(repository, noopObjectCleaner{}, nil, time.Now)
	principal := attestation.Principal{AppID: "health", KeyID: "key-1", DeviceID: "device-1"}
	if _, err := service.RecordConsents(context.Background(), principal, []Consent{
		{Scope: ManagedAIScope, DocumentVersion: AIDocumentVersion, Granted: true},
		{Scope: SensitiveHealthScope, DocumentVersion: AIDocumentVersion, Granted: false},
	}); err != nil {
		t.Fatal(err)
	}
	granted, err := service.HasRequiredConsents(
		context.Background(), principal, []string{ManagedAIScope, SensitiveHealthScope},
	)
	if err != nil || granted {
		t.Fatalf("expected sensitive AI consent to be denied: granted=%v err=%v", granted, err)
	}
	if _, err := service.RecordConsents(context.Background(), principal, []Consent{
		{Scope: SensitiveHealthScope, DocumentVersion: AIDocumentVersion, Granted: true},
	}); err != nil {
		t.Fatal(err)
	}
	granted, err = service.HasRequiredConsents(
		context.Background(), principal, []string{ManagedAIScope, SensitiveHealthScope},
	)
	if err != nil || !granted {
		t.Fatalf("expected both consents to be granted: granted=%v err=%v", granted, err)
	}
}

type noopObjectCleaner struct{}

func TestJournalImageConsentCannotBeGrantedByHealth(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil, nil, time.Now)
	principal := attestation.Principal{AppID: "health", KeyID: "key", DeviceID: "device"}
	consent := Consent{Scope: JournalIllustrationScope, DocumentVersion: JournalIllustrationDocumentVersion, Granted: true}
	if _, err := service.RecordConsents(context.Background(), principal, []Consent{consent}); err != ErrInvalidConsent {
		t.Fatal("health accepted journal consent")
	}
	principal.AppID = "journal"
	if _, err := service.RecordConsents(context.Background(), principal, []Consent{consent}); err != nil {
		t.Fatal(err)
	}
	if allowed, err := service.HasRequiredConsents(context.Background(), principal, []string{JournalIllustrationScope}); err != nil || !allowed {
		t.Fatal("journal grant unreadable")
	}
	principal.AppID = "health"
	if _, err := service.HasRequiredConsents(context.Background(), principal, []string{JournalIllustrationScope}); err != ErrInvalidConsent {
		t.Fatal("cross-app consent query")
	}
}

func (noopObjectCleaner) DeleteObject(context.Context, string) error { return nil }
