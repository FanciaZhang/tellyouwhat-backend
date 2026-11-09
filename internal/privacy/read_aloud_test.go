package privacy

import (
	"context"
	"github.com/tellyouwhat/backend/internal/attestation"
	"testing"
	"time"
)

func TestJournalCloudConsentIsUnifiedVersionedAppScopedAndRevocable(t *testing.T) {
	ctx := context.Background()
	repository := NewMemoryRepository()
	s := NewService(repository, noopObjectCleaner{}, nil, time.Now)
	p := attestation.Principal{AppID: "journal", KeyID: "journal-key", DeviceID: "device"}
	check := func(want bool) {
		t.Helper()
		for _, scope := range []string{ManagedAIScope, JournalIllustrationScope, JournalReadAloudScope} {
			got, err := s.HasRequiredConsents(ctx, p, []string{scope})
			if err != nil || got != want {
				t.Fatalf("%s consent=%v, want %v: %v", scope, got, want, err)
			}
		}
	}
	// Neither historical partial choices nor a partially written current decision grants any feature.
	for _, version := range []string{AIDocumentVersion, JournalCloudAIDocumentVersion} {
		if err := repository.RecordConsents(ctx, []Record{{KeyID: p.KeyID, DeviceID: p.DeviceID, Scope: ManagedAIScope, DocumentVersion: version, Granted: true, RecordedAt: time.Now()}}); err != nil {
			t.Fatal(err)
		}
		check(false)
	}
	for _, bad := range []struct {
		name   string
		values []Consent
	}{
		{"partial", JournalCloudAIConsents(true)[:1]},
		{"missing", JournalCloudAIConsents(true)[:2]},
		{"mixed", []Consent{{ManagedAIScope, JournalCloudAIDocumentVersion, true}, {JournalIllustrationScope, JournalCloudAIDocumentVersion, false}, {JournalReadAloudScope, JournalCloudAIDocumentVersion, true}}},
		{"duplicate", []Consent{{ManagedAIScope, JournalCloudAIDocumentVersion, true}, {ManagedAIScope, JournalCloudAIDocumentVersion, true}, {JournalReadAloudScope, JournalCloudAIDocumentVersion, true}}},
		{"old", []Consent{{ManagedAIScope, AIDocumentVersion, true}, {JournalIllustrationScope, "2026-09-27", true}, {JournalReadAloudScope, "2026-10-20", true}}},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if _, err := s.RecordConsents(ctx, p, bad.values); err != ErrInvalidConsent {
				t.Fatal("invalid decision accepted", err)
			}
			check(false)
		})
	}
	for _, app := range []string{"health", "birthday", "albums"} {
		other := p
		other.AppID = app
		if _, err := s.RecordConsents(ctx, other, JournalCloudAIConsents(true)); err != ErrInvalidConsent {
			t.Fatal("cross-app decision accepted", app, err)
		}
	}
	values := JournalCloudAIConsents(true)
	values[0], values[2] = values[2], values[0]
	if _, err := s.RecordConsents(ctx, p, values); err != nil {
		t.Fatal(err)
	}
	check(true)
	if _, err := s.RecordConsents(ctx, p, JournalCloudAIConsents(false)); err != nil {
		t.Fatal(err)
	}
	check(false)
	health := attestation.Principal{AppID: "health", KeyID: "health-key", DeviceID: "health-device"}
	if _, err := s.RecordConsents(ctx, health, []Consent{{ManagedAIScope, AIDocumentVersion, true}}); err != nil {
		t.Fatal("published Health consent changed", err)
	}
	if ok, err := s.HasRequiredConsents(ctx, health, []string{ManagedAIScope}); err != nil || !ok {
		t.Fatal("Health grant lost", ok, err)
	}
}
