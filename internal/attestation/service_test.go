package attestation

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func TestAuthenticateBindsAssertionToBodyAndAdvancesCounter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	nonces := NewMemoryNonceStore()
	keys := NewMemoryKeyStore()
	keys.Put(RegisteredKey{KeyID: "key-1", DeviceID: "device-1", TransactionID: "transaction-1", PublicKey: []byte("public"), Counter: 4})
	nonce, err := nonces.Issue(context.Background(), "key-1", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &fakeAssertionVerifier{nextCounter: 5}
	service := NewService(nonces, keys, verifier, func() time.Time { return now })

	principal, err := service.Authenticate(context.Background(), RequestProof{
		Method:     "POST",
		Path:       "/v1/ai/operations/meal_text_capture/responses",
		RequestID:  "request-1",
		KeyID:      "key-1",
		Assertion:  base64.StdEncoding.EncodeToString([]byte("assertion")),
		Nonce:      nonce,
		Timestamp:  now.Format(time.RFC3339),
		BodySHA256: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
	})
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if principal.DeviceID != "device-1" {
		t.Fatalf("unexpected principal: %+v", principal)
	}
	if len(verifier.lastClientDataHash) != 32 {
		t.Fatalf("clientDataHash must be SHA-256, got %d bytes", len(verifier.lastClientDataHash))
	}
	key, _ := keys.Get(context.Background(), "key-1")
	if key.Counter != 5 {
		t.Fatalf("counter was not advanced: %d", key.Counter)
	}
}

func TestAuthenticateRejectsReusedNonceAsConflict(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	nonces := NewMemoryNonceStore()
	keys := NewMemoryKeyStore()
	keys.Put(RegisteredKey{KeyID: "key-1", PublicKey: []byte("public")})
	nonce, _ := nonces.Issue(context.Background(), "key-1", time.Minute, now)
	service := NewService(nonces, keys, &fakeAssertionVerifier{nextCounter: 1}, func() time.Time { return now })
	proof := validProof(now, nonce)

	if _, err := service.Authenticate(context.Background(), proof); err != nil {
		t.Fatalf("first assertion failed: %v", err)
	}
	_, err := service.Authenticate(context.Background(), proof)
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("expected replay error, got %v", err)
	}
}

func TestAuthenticateRejectsStaleTimestampBeforeAssertionVerification(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 10, 0, 0, time.UTC)
	nonces := NewMemoryNonceStore()
	keys := NewMemoryKeyStore()
	keys.Put(RegisteredKey{KeyID: "key-1", PublicKey: []byte("public")})
	nonce, _ := nonces.Issue(context.Background(), "key-1", time.Minute, now)
	verifier := &fakeAssertionVerifier{nextCounter: 1}
	service := NewService(nonces, keys, verifier, func() time.Time { return now })
	proof := validProof(now.Add(-6*time.Minute), nonce)

	_, err := service.Authenticate(context.Background(), proof)
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("expected authentication error, got %v", err)
	}
	if verifier.calls != 0 {
		t.Fatalf("stale proof should not reach verifier")
	}
}

func TestAuthenticateRejectsNonMonotonicAssertionCounter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	nonces := NewMemoryNonceStore()
	keys := NewMemoryKeyStore()
	keys.Put(RegisteredKey{KeyID: "key-1", PublicKey: []byte("public"), Counter: 8})
	nonce, _ := nonces.Issue(context.Background(), "key-1", time.Minute, now)
	service := NewService(nonces, keys, &fakeAssertionVerifier{nextCounter: 8}, func() time.Time { return now })

	_, err := service.Authenticate(context.Background(), validProof(now, nonce))
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("expected replay error, got %v", err)
	}
}

func TestAuthenticatePreservesKeyStoreInfrastructureFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	service := NewService(NewMemoryNonceStore(), failingKeyStore{}, &fakeAssertionVerifier{}, func() time.Time { return now })
	_, err := service.Authenticate(context.Background(), validProof(now, "nonce"))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected retryable infrastructure error, got %v", err)
	}
}

func validProof(now time.Time, nonce string) RequestProof {
	return RequestProof{
		Method:     "POST",
		Path:       "/v1/ai/operations/meal_text_capture/responses",
		RequestID:  "request-1",
		KeyID:      "key-1",
		Assertion:  base64.StdEncoding.EncodeToString([]byte("assertion")),
		Nonce:      nonce,
		Timestamp:  now.Format(time.RFC3339),
		BodySHA256: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
	}
}

type fakeAssertionVerifier struct {
	nextCounter        uint32
	calls              int
	lastClientDataHash []byte
}

type failingKeyStore struct{}

func (failingKeyStore) Get(context.Context, string) (RegisteredKey, error) {
	return RegisteredKey{}, errors.New("database unavailable")
}

func (failingKeyStore) AdvanceCounter(context.Context, string, uint32, uint32) error {
	return errors.New("database unavailable")
}

func (verifier *fakeAssertionVerifier) VerifyAssertion(_ []byte, _ []byte, clientDataHash []byte) (uint32, error) {
	verifier.calls++
	verifier.lastClientDataHash = append([]byte(nil), clientDataHash...)
	return verifier.nextCounter, nil
}
