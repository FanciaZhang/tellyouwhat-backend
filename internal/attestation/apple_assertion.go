package attestation

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"strings"

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
		return 0, diagnosticFailure("dependencies", ErrAuthentication)
	}
	var value map[string]cbor.RawMessage
	if err := cbor.Unmarshal(assertion, &value); err != nil || len(value) != 2 {
		return 0, diagnosticFailure("assertion_cbor", ErrAuthentication)
	}
	var authenticatorData []byte
	if err := cbor.Unmarshal(value["authenticatorData"], &authenticatorData); err != nil {
		return 0, diagnosticFailure("authenticator_data", ErrAuthentication)
	}
	var signature []byte
	if err := cbor.Unmarshal(value["signature"], &signature); err != nil {
		return 0, diagnosticFailure("signature_encoding", ErrAuthentication)
	}
	if len(authenticatorData) < 37 || !bytes.Equal(authenticatorData[:32], verifier.rpIDHash[:]) {
		return 0, diagnosticFailure("rp_id", ErrAuthentication)
	}
	if err := validateAssertionExtensions(authenticatorData); err != nil {
		return 0, diagnosticFailure("extensions", ErrAuthentication)
	}
	parsedKey, err := x509.ParsePKIXPublicKey(publicKeyDER)
	if err != nil {
		return 0, diagnosticFailure("public_key", ErrAuthentication)
	}
	publicKey, ok := parsedKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve.Params().Name != "P-256" {
		return 0, diagnosticFailure("public_key", ErrAuthentication)
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
		return 0, diagnosticFailure("signature", ErrAuthentication)
	}
	return binary.BigEndian.Uint32(authenticatorData[33:37]), nil
}

func validateAssertionExtensions(authenticatorData []byte) error {
	const extensionFlag = 0x80
	// Apple can set AT and ED on simplified App Attest assertions. Validate
	// the actual assertion layout instead of requiring the AT bit to be clear;
	// a full credential section will not decode as the extension map below.
	if len(authenticatorData) == 37 {
		if authenticatorData[32]&extensionFlag != 0 {
			return ErrAuthentication
		}
		return nil
	}
	if len(authenticatorData) > 16*1024 {
		return ErrAuthentication
	}
	decoder, err := (cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF}).DecMode()
	if err != nil {
		return ErrAuthentication
	}
	var extensions map[string]cbor.RawMessage
	if err := decoder.Unmarshal(authenticatorData[37:], &extensions); err != nil || len(extensions) == 0 {
		return ErrAuthentication
	}
	if raw, ok := extensions["apple_validation_category_01"]; ok {
		var categoryBytes []byte
		if err := decoder.Unmarshal(raw, &categoryBytes); err != nil || len(categoryBytes) != 4 {
			return ErrAuthentication
		}
		category := binary.LittleEndian.Uint32(categoryBytes)
		if category < 2 || category > 5 {
			return ErrAuthentication
		}
	}
	if raw, ok := extensions["apple_bundle_version_01"]; ok {
		var version string
		if err := decoder.Unmarshal(raw, &version); err != nil || strings.TrimSpace(version) == "" || len(version) > 128 {
			return ErrAuthentication
		}
	}
	// Keep the complete authenticator data in the signed nonce, including
	// extensions. Parsing must never strip fields before signature verification.
	return nil
}

var _ AssertionVerifier = (*AppleAssertionVerifier)(nil)
