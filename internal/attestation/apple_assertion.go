package attestation

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"

	"github.com/fxamacker/cbor/v2"
)

type AppleAssertionVerifier struct {
	rpIDHash [32]byte
}

func NewAppleAssertionVerifier(teamID, bundleID string) *AppleAssertionVerifier {
	return &AppleAssertionVerifier{
		rpIDHash: sha256.Sum256([]byte(teamID + "." + bundleID)),
	}
}

func (verifier *AppleAssertionVerifier) VerifyAssertion(
	publicKeyDER,
	assertion,
	clientDataHash []byte,
) (uint32, error) {
	if verifier == nil || len(clientDataHash) != sha256.Size {
		return 0, ErrAuthentication
	}
	var value map[string]cbor.RawMessage
	if err := cbor.Unmarshal(assertion, &value); err != nil || len(value) != 2 {
		return 0, ErrAuthentication
	}
	var authenticatorData []byte
	if err := cbor.Unmarshal(value["authenticatorData"], &authenticatorData); err != nil {
		return 0, ErrAuthentication
	}
	var signature []byte
	if err := cbor.Unmarshal(value["signature"], &signature); err != nil {
		return 0, ErrAuthentication
	}
	if len(authenticatorData) != 37 || !bytes.Equal(authenticatorData[:32], verifier.rpIDHash[:]) {
		return 0, ErrAuthentication
	}
	parsedKey, err := x509.ParsePKIXPublicKey(publicKeyDER)
	if err != nil {
		return 0, ErrAuthentication
	}
	publicKey, ok := parsedKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve.Params().Name != "P-256" {
		return 0, ErrAuthentication
	}
	nonceInput := make([]byte, 0, len(authenticatorData)+len(clientDataHash))
	nonceInput = append(nonceInput, authenticatorData...)
	nonceInput = append(nonceInput, clientDataHash...)
	nonce := sha256.Sum256(nonceInput)
	// Apple signs nonce as an ECDSA-SHA256 message. VerifyASN1 expects the
	// message's digest, not the message itself.
	// https://developer.apple.com/documentation/devicecheck/validating-apps-that-connect-to-your-server
	digest := sha256.Sum256(nonce[:])
	if !ecdsa.VerifyASN1(publicKey, digest[:], signature) {
		return 0, ErrAuthentication
	}
	return binary.BigEndian.Uint32(authenticatorData[33:37]), nil
}

var _ AssertionVerifier = (*AppleAssertionVerifier)(nil)
