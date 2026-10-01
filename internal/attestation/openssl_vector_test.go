package attestation

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
)

// A frozen proof produced by a separate signing implementation prevents the
// verifier and dynamic test signer from silently agreeing on a wrong digest.
func TestIndependentOpenSSLAppStoreVectorAuthenticatesBoundRequest(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/appstore-openssl-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Binding           contracts.RequestBinding `json:"binding"`
		PublicKeyDER      string                   `json:"publicKeyDER"`
		ClientDataHashHex string                   `json:"clientDataHashHex"`
		Assertion         string                   `json:"assertion"`
		Counter           uint32                   `json:"counter"`
	}
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatal(err)
	}
	if got := contracts.RequestBindingDigest(vector.Binding); got != vector.ClientDataHashHex {
		t.Fatal("server request binding differs from the independent client vector")
	}
	publicKey, err := base64.StdEncoding.DecodeString(vector.PublicKeyDER)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := base64.StdEncoding.DecodeString(vector.Assertion)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hex.DecodeString(vector.ClientDataHashHex)
	if err != nil {
		t.Fatal(err)
	}
	verifier := NewAppleAssertionVerifier("TEAM", "cn.tellyouwhat.healthapp")
	if counter, err := verifier.VerifyAssertion(publicKey, assertion, hash); err != nil || counter != vector.Counter {
		t.Fatalf("independent signature vector rejected: counter=%d err=%v", counter, err)
	}
	now, err := time.Parse(time.RFC3339, vector.Binding.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	nonces, keys := NewMemoryNonceStore(), NewMemoryKeyStore()
	nonces.entries[vector.Binding.Nonce] = nonceEntry{keyID: "synthetic-vector-key", expiresAt: now.Add(time.Minute)}
	keys.Put(RegisteredKey{AppID: "health", KeyID: "synthetic-vector-key", DeviceID: "synthetic-device", Environment: "production", PublicKey: publicKey})
	service := NewService(nonces, keys, verifier, func() time.Time { return now }).RequireEnvironment(EnvironmentProduction)
	principal, err := service.Authenticate(context.Background(), RequestProof{
		Method: vector.Binding.Method, Path: vector.Binding.Path, RequestID: vector.Binding.RequestID,
		KeyID: "synthetic-vector-key", Assertion: vector.Assertion, Nonce: vector.Binding.Nonce, Timestamp: vector.Binding.Timestamp, BodySHA256: vector.Binding.BodySHA256})
	if err != nil || principal.DeviceID != "synthetic-device" {
		t.Fatalf("independent request vector did not authenticate: %v", err)
	}
	stored, err := keys.Get(context.Background(), "synthetic-vector-key")
	if err != nil || stored.Counter != vector.Counter {
		t.Fatal("independent counter was not committed")
	}
}
