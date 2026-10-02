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

func (noopObjectCleaner) DeleteObject(context.Context, string) error { return nil }

func TestHealthEligibilityRequiresCompleteVersionPair(t *testing.T) {
	cases := []struct {
		name     string
		consents []Consent
		want     bool
	}{
		{"legacy", []Consent{{AdultScope, GeneralDocumentVersion, true}, {PrivacyTermsScope, GeneralDocumentVersion, true}}, true},
		{"current", []Consent{{Age14PlusScope, HealthGeneralDocumentVersion, true}, {PrivacyTermsScope, HealthGeneralDocumentVersion, true}}, true},
		{"mixed", []Consent{{Age14PlusScope, HealthGeneralDocumentVersion, true}, {PrivacyTermsScope, GeneralDocumentVersion, true}}, false},
		{"revoked", []Consent{{Age14PlusScope, HealthGeneralDocumentVersion, false}, {PrivacyTermsScope, HealthGeneralDocumentVersion, true}}, false},
		{"missingAge", []Consent{{PrivacyTermsScope, HealthGeneralDocumentVersion, true}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repository := NewMemoryRepository()
			service := NewService(repository, noopObjectCleaner{}, nil, time.Now)
			principal := attestation.Principal{AppID: "health", KeyID: "key", DeviceID: "device"}
			if _, err := service.RecordConsents(context.Background(), principal, tc.consents); err != nil {
				t.Fatal(err)
			}
			got, err := service.HasRequiredConsents(context.Background(), principal, []string{HealthEligibilityScope})
			if err != nil || got != tc.want {
				t.Fatalf("got=%v want=%v err=%v", got, tc.want, err)
			}
		})
	}
}

func TestNewHealthConsentDoesNotAuthorizeJournal(t *testing.T) {
	service := NewService(NewMemoryRepository(), noopObjectCleaner{}, nil, time.Now)
	principal := attestation.Principal{AppID: "journal", KeyID: "key", DeviceID: "device"}
	if _, err := service.RecordConsents(context.Background(), principal, []Consent{{Age14PlusScope, HealthGeneralDocumentVersion, true}}); err != ErrInvalidConsent {
		t.Fatalf("got %v", err)
	}
}

func TestHealthUpgradeRevocationCannotFallBackToAdult(t *testing.T) {
	service := NewService(NewMemoryRepository(), noopObjectCleaner{}, nil, time.Now)
	principal := attestation.Principal{AppID: "health", KeyID: "key", DeviceID: "device"}
	_, err := service.RecordConsents(context.Background(), principal, []Consent{{AdultScope, GeneralDocumentVersion, true}, {PrivacyTermsScope, GeneralDocumentVersion, true}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.RecordConsents(context.Background(), principal, []Consent{{AdultScope, GeneralDocumentVersion, false}, {Age14PlusScope, HealthGeneralDocumentVersion, false}, {PrivacyTermsScope, HealthGeneralDocumentVersion, true}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.HasRequiredConsents(context.Background(), principal, []string{HealthEligibilityScope})
	if err != nil || got {
		t.Fatalf("revocation bypassed: got=%v err=%v", got, err)
	}
}
