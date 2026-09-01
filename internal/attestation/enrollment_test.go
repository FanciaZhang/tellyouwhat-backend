package attestation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
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
		Environment:       EnvironmentDevelopment,
		DevelopmentSecret: "rotate-me",
		AllowedBuilds:     map[string]struct{}{"100": {}},
	}, nonces, keys, verifier, func() time.Time { return now })
	challenge, err := enrollment.IssueChallenge(context.Background(), "key-1")
	if err != nil {
		t.Fatal(err)
	}
	request := RegistrationRequest{
		KeyID:            "key-1",
		Challenge:        challenge.Value,
		Attestation:      base64.StdEncoding.EncodeToString([]byte("attestation")),
		Build:            "100",
		ActivationSecret: "rotate-me",
	}
	principal, err := enrollment.Register(context.Background(), request)
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
	replayed, err := enrollment.Register(context.Background(), request)
	if err != nil {
		t.Fatalf("recover registered key after a lost response: %v", err)
	}
	if replayed != principal {
		t.Fatalf("replayed registration returned another principal: got %+v want %+v", replayed, principal)
	}
	if verifier.calls != 1 {
		t.Fatalf("replayed registration reverified a one-time attestation: %d", verifier.calls)
	}
}

func TestEnrollmentReplayDoesNotSucceedWhenKeyWasNeverStored(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	nonces := NewMemoryNonceStore()
	keys := failingEnrollmentKeyStore{}
	enrollment := NewEnrollmentService(EnrollmentConfig{
		Environment:       EnvironmentDevelopment,
		DevelopmentSecret: "rotate-me",
		AllowedBuilds:     map[string]struct{}{"100": {}},
	}, nonces, keys, &fakeAttestationObjectVerifier{}, func() time.Time { return now })
	challenge, err := enrollment.IssueChallenge(context.Background(), "key-1")
	if err != nil {
		t.Fatal(err)
	}
	request := RegistrationRequest{
		KeyID:            "key-1",
		Challenge:        challenge.Value,
		Attestation:      base64.StdEncoding.EncodeToString([]byte("attestation")),
		Build:            "100",
		ActivationSecret: "rotate-me",
	}
	if _, err := enrollment.Register(context.Background(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first registration error = %v, want unavailable", err)
	}
	if _, err := enrollment.Register(context.Background(), request); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay without a stored key = %v, want replay", err)
	}
}

type fakeAttestationObjectVerifier struct {
	clientDataHash []byte
	calls          int
}

func (verifier *fakeAttestationObjectVerifier) Verify(_ string, _ []byte, clientDataHash []byte) (VerifiedAttestation, error) {
	verifier.calls++
	verifier.clientDataHash = append([]byte(nil), clientDataHash...)
	return VerifiedAttestation{PublicKey: []byte("verified-public-key"), Receipt: []byte("receipt")}, nil
}

type failingEnrollmentKeyStore struct{}

func (failingEnrollmentKeyStore) Register(context.Context, RegisteredKey) error {
	return errors.New("database unavailable")
}

func (failingEnrollmentKeyStore) Get(context.Context, string) (RegisteredKey, error) {
	return RegisteredKey{}, ErrKeyNotFound
}
