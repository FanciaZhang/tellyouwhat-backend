package privacy

import (
	"context"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
)

func TestReadAloudConsentIsIndependentAppScopedVersionedAndRevocable(t *testing.T) {
	s := NewService(NewMemoryRepository(), noopObjectCleaner{}, nil, time.Now)
	ctx := context.Background()
	p := attestation.Principal{AppID: "journal", KeyID: "journal-key", DeviceID: "device"}
	if _, err := s.RecordConsents(ctx, p, []Consent{{ManagedAIScope, AIDocumentVersion, true}, {JournalIllustrationScope, JournalIllustrationDocumentVersion, true}}); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		got, err := s.HasRequiredConsents(ctx, p, []string{JournalReadAloudScope})
		if err != nil || got != want {
			t.Fatal("wrong independent TTS decision", got, err)
		}
	}
	check(false)
	grant := Consent{JournalReadAloudScope, JournalReadAloudDocumentVersion, true}
	for _, app := range []string{"health", "birthday", "albums"} {
		other := p
		other.AppID = app
		if _, err := s.RecordConsents(ctx, other, []Consent{grant}); err != ErrInvalidConsent {
			t.Fatal("cross-app grant accepted", app, err)
		}
		if _, err := s.HasRequiredConsents(ctx, other, []string{JournalReadAloudScope}); err != ErrInvalidConsent {
			t.Fatal("cross-app read accepted", app, err)
		}
	}
	invalid := grant
	invalid.DocumentVersion = AIDocumentVersion
	if _, err := s.RecordConsents(ctx, p, []Consent{invalid}); err != ErrInvalidConsent {
		t.Fatal("wrong document accepted", err)
	}
	if _, err := s.RecordConsents(ctx, p, []Consent{grant}); err != nil {
		t.Fatal(err)
	}
	check(true)
	grant.Granted = false
	if _, err := s.RecordConsents(ctx, p, []Consent{grant}); err != nil {
		t.Fatal(err)
	}
	check(false)
	if got, err := s.HasRequiredConsents(ctx, p, []string{ManagedAIScope, JournalIllustrationScope}); err != nil || !got {
		t.Fatal("TTS withdrawal removed unrelated consents", got, err)
	}
}
