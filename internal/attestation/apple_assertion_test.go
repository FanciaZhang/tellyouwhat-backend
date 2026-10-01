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

func TestAppleAssertionVerifierSupportsSignedAuthenticatorExtensions(t *testing.T) {
	t.Parallel()
	extension, err := cbor.Marshal(map[string]any{
		"apple_validation_category_01": []byte{4, 0, 0, 0},
		"apple_bundle_version_01":      "1200",
	})
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	publicKey, _ := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	requestHash := sha256.Sum256([]byte("production request"))
	authData := make([]byte, 37)
	rp := sha256.Sum256([]byte("TEAMID.cn.tellyouwhat.healthapp"))
	copy(authData, rp[:])
	// App Store assertions can set both AT and ED while containing only
	// the 37-byte assertion header and the signed extension dictionary.
	authData[32] = 0xc0
	binary.BigEndian.PutUint32(authData[33:37], 8)
	authData = append(authData, extension...)
	sign := func(data []byte, digestHash [32]byte) []byte {
		nonce := sha256.Sum256(append(append([]byte(nil), data...), digestHash[:]...))
		digest := sha256.Sum256(nonce[:])
		signature, err := ecdsa.SignASN1(rand.Reader, privateKey, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		assertion, err := cbor.Marshal(map[string]any{"authenticatorData": data, "signature": signature})
		if err != nil {
			t.Fatal(err)
		}
		return assertion
	}
	verifier := NewAppleAssertionVerifier("TEAMID", "cn.tellyouwhat.healthapp")
	assertion := sign(authData, requestHash)
	if counter, err := verifier.VerifyAssertion(publicKey, assertion, requestHash[:]); err != nil || counter != 8 {
		t.Fatalf("signed App Store assertion with flags 0xc0 rejected: counter=%d err=%v", counter, err)
	}
	wrongHash := sha256.Sum256([]byte("another request"))
	if _, err := verifier.VerifyAssertion(publicKey, assertion, wrongHash[:]); err == nil {
		t.Fatal("extensions must not allow a signature for another request")
	}
	if _, err := NewAppleAssertionVerifier("TEAMID", "another.app").VerifyAssertion(publicKey, assertion, requestHash[:]); err == nil {
		t.Fatal("extensions must not allow another App ID")
	}
	malformed := []struct {
		name string
		data []byte
	}{
		{"extension flag without data", append([]byte(nil), authData[:37]...)},
		{"truncated CBOR", append([]byte(nil), authData[:len(authData)-1]...)},
		{"trailing CBOR", append(append([]byte(nil), authData...), 0xa0)},
		{"extension is not map", append(append([]byte(nil), authData[:37]...), 0x80)},
	}

	for _, item := range []struct {
		name  string
		value any
	}{
		{"invalid category", map[string]any{"apple_validation_category_01": []byte{0, 0, 0, 0}}},
		{"unknown category", map[string]any{"apple_validation_category_01": []byte{10, 0, 0, 0}}},
		{"category has wrong type", map[string]any{"apple_validation_category_01": "4"}},
		{"empty bundle version", map[string]any{"apple_bundle_version_01": ""}},
		{"bundle version has wrong type", map[string]any{"apple_bundle_version_01": 1200}},
	} {
		encoded, err := cbor.Marshal(item.value)
		if err != nil {
			t.Fatal(err)
		}
		malformed = append(malformed, struct {
			name string
			data []byte
		}{item.name, append(append([]byte(nil), authData[:37]...), encoded...)})
	}
	duplicateKey := append([]byte{0xa2, 0x61, 'x', 0x01, 0x61, 'x', 0x02}, []byte{}...)
	malformed = append(malformed, struct {
		name string
		data []byte
	}{"duplicate extension keys", append(append([]byte(nil), authData[:37]...), duplicateKey...)})
	noFlag := append([]byte(nil), authData...)
	noFlag[32] = 0x01
	if counter, err := verifier.VerifyAssertion(publicKey, sign(noFlag, requestHash), requestHash[:]); err != nil || counter != 8 {
		t.Fatalf("signed Apple extension dictionary without the WebAuthn extension flag rejected: counter=%d err=%v", counter, err)
	}
	edOnly := append([]byte(nil), authData...)
	edOnly[32] = 0x81
	if counter, err := verifier.VerifyAssertion(publicKey, sign(edOnly, requestHash), requestHash[:]); err != nil || counter != 8 {
		t.Fatalf("signed assertion with only the extension flag rejected: counter=%d err=%v", counter, err)
	}
	// A full WebAuthn credential section is not an assertion extension map,
	// even when AT is set and the complete bytes have a valid signature.
	credentialData := append(append([]byte(nil), authData[:37]...), []byte("appattest\x00\x00\x00\x00\x00\x00\x00")...)
	credentialData = append(credentialData, 0, 32)
	credentialData = append(credentialData, make([]byte, 32)...)
	credentialData = append(credentialData, 0xa0)
	credentialData = append(credentialData, extension...)
	malformed = append(malformed, struct {
		name string
		data []byte
	}{"credential section is not an assertion", credentialData})
	for _, test := range malformed {
		t.Run(test.name, func(t *testing.T) {
			if _, err := verifier.VerifyAssertion(publicKey, sign(test.data, requestHash), requestHash[:]); err == nil {
				t.Fatal("malformed authenticator extensions accepted even with a valid signature")
			}
		})
	}
	var decoded map[string]cbor.RawMessage
	if err := cbor.Unmarshal(assertion, &decoded); err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), authData...)
	tampered[len(tampered)-1] ^= 1
	decoded["authenticatorData"], _ = cbor.Marshal(tampered)
	changed, _ := cbor.Marshal(decoded)
	if _, err := verifier.VerifyAssertion(publicKey, changed, requestHash[:]); err == nil {
		t.Fatal("modified extensions must invalidate the signature")
	}
}
