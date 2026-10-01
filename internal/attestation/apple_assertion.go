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

const (
	assertionHeaderSize      = 37
	maxAuthenticatorDataSize = 16 * 1024
	maxAssertionSize         = maxAuthenticatorDataSize + 1024
)

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
		return 0, diagnosticFailure(stageDependencies, ErrAuthentication)
	}
	var value map[string]cbor.RawMessage
	if len(assertion) > maxAssertionSize {
		return 0, diagnosticFailure(stageAssertionCBOR, ErrAuthentication)
	}
	if err := proofCBOR.Unmarshal(assertion, &value); err != nil || len(value) != 2 {
		return 0, diagnosticFailure(stageAssertionCBOR, ErrAuthentication)
	}
	var authenticatorData []byte
	if err := proofCBOR.Unmarshal(value["authenticatorData"], &authenticatorData); err != nil {
		return 0, diagnosticFailure(stageAuthenticatorData, ErrAuthentication)
	}
	var signature []byte
	if err := proofCBOR.Unmarshal(value["signature"], &signature); err != nil {
		return 0, diagnosticFailure(stageSignatureEncoding, ErrAuthentication)
	}
	if len(authenticatorData) < assertionHeaderSize || !bytes.Equal(authenticatorData[:32], verifier.rpIDHash[:]) {
		return 0, diagnosticFailure(stageRPID, ErrAuthentication)
	}
	if err := validateAssertionExtensions(authenticatorData); err != nil {
		return 0, diagnosticFailure(stageExtensions, ErrAuthentication)
	}
	parsedKey, err := x509.ParsePKIXPublicKey(publicKeyDER)
	if err != nil {
		return 0, diagnosticFailure(stagePublicKey, ErrAuthentication)
	}
	publicKey, ok := parsedKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve.Params().Name != "P-256" {
		return 0, diagnosticFailure(stagePublicKey, ErrAuthentication)
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
		return 0, diagnosticFailure(stageSignature, ErrAuthentication)
	}
	return binary.BigEndian.Uint32(authenticatorData[33:assertionHeaderSize]), nil
}

func validateAssertionExtensions(authenticatorData []byte) error {
	const extensionFlag = 0x80
	// Apple can set AT and ED on simplified App Attest assertions. Validate
	// the actual assertion layout instead of requiring the AT bit to be clear;
	// a full credential section will not decode as the extension map below.
	if len(authenticatorData) < assertionHeaderSize {
		return ErrAuthentication
	}
	if len(authenticatorData) == assertionHeaderSize {
		if authenticatorData[32]&extensionFlag != 0 {
			return ErrAuthentication
		}
		return nil
	}
	if len(authenticatorData) > maxAuthenticatorDataSize {
		return ErrAuthentication
	}
	var extensions map[string]cbor.RawMessage
	if err := proofCBOR.Unmarshal(authenticatorData[assertionHeaderSize:], &extensions); err != nil || len(extensions) == 0 {
		return ErrAuthentication
	}
	var category uint32
	for _, name := range []string{"apple_validation_category_01", "validationCategory"} {
		raw, ok := extensions[name]
		if !ok {
			continue
		}
		var categoryBytes []byte
		if err := proofCBOR.Unmarshal(raw, &categoryBytes); err != nil || len(categoryBytes) != 4 {
			return ErrAuthentication
		}
		value := binary.LittleEndian.Uint32(categoryBytes)
		if value < 2 || value > 5 || (category != 0 && value != category) {
			return ErrAuthentication
		}
		category = value
	}
	var version string
	for _, name := range []string{"apple_bundle_version_01", "bundleVersion"} {
		raw, ok := extensions[name]
		if !ok {
			continue
		}
		var value string
		if err := proofCBOR.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" || len(value) > 128 || (version != "" && value != version) {
			return ErrAuthentication
		}
		version = value
	}
	// Keep the complete authenticator data in the signed nonce, including
	// extensions. Parsing must never strip fields before signature verification.
	return nil
}

var _ AssertionVerifier = (*AppleAssertionVerifier)(nil)
