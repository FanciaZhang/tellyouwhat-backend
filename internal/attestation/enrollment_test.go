package attestation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

func TestDevelopmentEnrollmentConsumesChallengeAndPinsVerifiedKey(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	nonces := NewMemoryNonceStore()
	keys := NewMemoryKeyStore()
	verifier := &fakeAttestationObjectVerifier{}
	enrollment := NewEnrollmentService(EnrollmentConfig{
		AppID:             "health",
		Environment:       EnvironmentDevelopment,
		DevelopmentSecret: "rotate-me",
		AllowedBuilds:     map[string]struct{}{"100": {}},
	}, nonces, keys, verifier, func() time.Time { return now })
	challenge, err := enrollment.IssueChallenge(context.Background(), "key-1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := enrollment.Register(context.Background(), RegistrationRequest{
		KeyID:            "key-1",
		Challenge:        challenge.Value,
		Attestation:      base64.StdEncoding.EncodeToString([]byte("attestation")),
		Build:            "100",
		ActivationSecret: "rotate-me",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	key, err := keys.Get(context.Background(), "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if key.DeviceID != principal.DeviceID || string(key.PublicKey) != "verified-public-key" {
		t.Fatalf("verified key was not pinned: %+v", key)
	}
	if len(verifier.clientDataHash) != sha256.Size {
		t.Fatalf("challenge was not hashed: %d", len(verifier.clientDataHash))
	}
}

type fakeAttestationObjectVerifier struct{ clientDataHash []byte }

func (verifier *fakeAttestationObjectVerifier) Verify(_ string, _ []byte, clientDataHash []byte) (VerifiedAttestation, error) {
	verifier.clientDataHash = append([]byte(nil), clientDataHash...)
	return VerifiedAttestation{PublicKey: []byte("verified-public-key"), Receipt: []byte("receipt")}, nil
}
