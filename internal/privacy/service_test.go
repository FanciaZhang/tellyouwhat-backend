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

type noopObjectCleaner struct{}

func (noopObjectCleaner) DeleteObject(context.Context, string) error { return nil }
