package attestation

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

func TestAppleAssertionVerifierValidatesSignatureRPIDAndCounter(t *testing.T) {
	t.Parallel()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	clientDataHash := sha256.Sum256([]byte("bound request"))
	authenticatorData := make([]byte, 37)
	rpIDHash := sha256.Sum256([]byte("TEAMID.cn.tellyouwhat.healthapp"))
	copy(authenticatorData[:32], rpIDHash[:])
	authenticatorData[32] = 0x01
	binary.BigEndian.PutUint32(authenticatorData[33:37], 7)
	nonceInput := append(append([]byte(nil), authenticatorData...), clientDataHash[:]...)
	nonce := sha256.Sum256(nonceInput)
	// App Attest signs the nonce as an ECDSA-SHA256 message. SignASN1 takes
	// the message digest, so the nonce must be hashed before signing.
	signedDigest := sha256.Sum256(nonce[:])
	signature, err := ecdsa.SignASN1(rand.Reader, privateKey, signedDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := cbor.Marshal(map[string]any{
		"authenticatorData": authenticatorData,
		"signature":         signature,
	})
	if err != nil {
		t.Fatal(err)
	}

	verifier := NewAppleAssertionVerifier("TEAMID", "cn.tellyouwhat.healthapp")
	counter, err := verifier.VerifyAssertion(publicKey, assertion, clientDataHash[:])
	if err != nil {
		t.Fatalf("verify assertion: %v", err)
	}
	if counter != 7 {
		t.Fatalf("unexpected counter: %d", counter)
	}
	if _, err := NewAppleAssertionVerifier("TEAMID", "another.app").VerifyAssertion(publicKey, assertion, clientDataHash[:]); err == nil {
		t.Fatal("an assertion from another app must be rejected")
	}

	// Hashing authData || clientDataHash only once is not Apple's signature
	// protocol. Do not accept it as a fallback when verification fails.
	singleHashSignature, err := ecdsa.SignASN1(rand.Reader, privateKey, nonce[:])
	if err != nil {
		t.Fatal(err)
	}
	singleHashAssertion, err := cbor.Marshal(map[string]any{
		"authenticatorData": authenticatorData,
		"signature":         singleHashSignature,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.VerifyAssertion(publicKey, singleHashAssertion, clientDataHash[:]); err == nil {
		t.Fatal("a signature over the wrong digest must be rejected")
	}
}

func TestAppleAssertionVerifierRejectsTamperedClientDataHash(t *testing.T) {
	t.Parallel()

	privateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	publicKey, _ := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	validHash := sha256.Sum256([]byte("valid"))
	authenticatorData := make([]byte, 37)
	rpIDHash := sha256.Sum256([]byte("TEAMID.cn.tellyouwhat.healthapp"))
	copy(authenticatorData, rpIDHash[:])
	binary.BigEndian.PutUint32(authenticatorData[33:], 1)
	nonce := sha256.Sum256(append(authenticatorData, validHash[:]...))
	signedDigest := sha256.Sum256(nonce[:])
	signature, _ := ecdsa.SignASN1(rand.Reader, privateKey, signedDigest[:])
	assertion, _ := cbor.Marshal(map[string]any{"authenticatorData": authenticatorData, "signature": signature})
	tamperedHash := sha256.Sum256([]byte("tampered"))

	verifier := NewAppleAssertionVerifier("TEAMID", "cn.tellyouwhat.healthapp")
	if _, err := verifier.VerifyAssertion(publicKey, assertion, tamperedHash[:]); err == nil {
		t.Fatal("tampered request hash must be rejected")
	}
}
