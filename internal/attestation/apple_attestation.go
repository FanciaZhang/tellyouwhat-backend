package attestation

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"time"

	"github.com/fxamacker/cbor/v2"
)

var appleAttestationNonceOID = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}

type Environment string

const (
	EnvironmentDevelopment Environment = "development"
	EnvironmentProduction  Environment = "production"
)

type VerifiedAttestation struct {
	PublicKey []byte
	Receipt   []byte
}

type AppleAttestationVerifier struct {
	rpIDHash    [32]byte
	environment Environment
	roots       *x509.CertPool
	now         func() time.Time
}

func NewAppleAttestationVerifier(
	teamID,
	bundleID string,
	environment Environment,
	roots *x509.CertPool,
) *AppleAttestationVerifier {
	return &AppleAttestationVerifier{
		rpIDHash:    sha256.Sum256([]byte(teamID + "." + bundleID)),
		environment: environment,
		roots:       roots,
		now:         time.Now,
	}
}

func (verifier *AppleAttestationVerifier) Verify(
	keyID string,
	attestationObject,
	clientDataHash []byte,
) (VerifiedAttestation, error) {
	if verifier == nil || verifier.roots == nil || len(clientDataHash) != sha256.Size {
		return VerifiedAttestation{}, ErrAuthentication
	}
	var object struct {
		Format   string `cbor:"fmt"`
		AuthData []byte `cbor:"authData"`
		AttStmt  struct {
			X5C     [][]byte `cbor:"x5c"`
			Receipt []byte   `cbor:"receipt"`
		} `cbor:"attStmt"`
	}
	if err := cbor.Unmarshal(attestationObject, &object); err != nil || object.Format != "apple-appattest" {
		return VerifiedAttestation{}, ErrAuthentication
	}
	if len(object.AttStmt.X5C) < 2 || len(object.AttStmt.Receipt) == 0 {
		return VerifiedAttestation{}, ErrAuthentication
	}
	leaf, err := x509.ParseCertificate(object.AttStmt.X5C[0])
	if err != nil {
		return VerifiedAttestation{}, ErrAuthentication
	}
	intermediates := x509.NewCertPool()
	for _, certificateDER := range object.AttStmt.X5C[1:] {
		certificate, parseErr := x509.ParseCertificate(certificateDER)
		if parseErr != nil {
			return VerifiedAttestation{}, ErrAuthentication
		}
		intermediates.AddCert(certificate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         verifier.roots,
		Intermediates: intermediates,
		CurrentTime:   verifier.now(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return VerifiedAttestation{}, ErrAuthentication
	}
	if err := verifier.verifyAuthenticatorData(object.AuthData, keyID); err != nil {
		return VerifiedAttestation{}, err
	}
	nonceInput := make([]byte, 0, len(object.AuthData)+len(clientDataHash))
	nonceInput = append(nonceInput, object.AuthData...)
	nonceInput = append(nonceInput, clientDataHash...)
	expectedNonce := sha256.Sum256(nonceInput)
	certificateNonce, err := attestationCertificateNonce(leaf)
	if err != nil || !bytes.Equal(certificateNonce, expectedNonce[:]) {
		return VerifiedAttestation{}, ErrAuthentication
	}
	publicKey, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve.Params().Name != "P-256" {
		return VerifiedAttestation{}, ErrAuthentication
	}
	publicKeyX963, err := publicKey.Bytes()
	if err != nil {
		return VerifiedAttestation{}, ErrAuthentication
	}
	publicKeyDigest := sha256.Sum256(publicKeyX963)
	decodedKeyID, err := decodeBase64(keyID)
	if err != nil || !bytes.Equal(decodedKeyID, publicKeyDigest[:]) {
		return VerifiedAttestation{}, ErrAuthentication
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return VerifiedAttestation{}, ErrAuthentication
	}
	return VerifiedAttestation{
		PublicKey: publicKeyDER,
		Receipt:   append([]byte(nil), object.AttStmt.Receipt...),
	}, nil
}

func (verifier *AppleAttestationVerifier) verifyAuthenticatorData(authData []byte, keyID string) error {
	if len(authData) < 55 || !bytes.Equal(authData[:32], verifier.rpIDHash[:]) || authData[32]&0x40 == 0 {
		return ErrAuthentication
	}
	if binary.BigEndian.Uint32(authData[33:37]) != 0 {
		return ErrAuthentication
	}
	expectedAAGUID := append([]byte("appattest"), make([]byte, 7)...)
	if verifier.environment == EnvironmentDevelopment {
		expectedAAGUID = []byte("appattestdevelop")
	}
	if !bytes.Equal(authData[37:53], expectedAAGUID) {
		return ErrAuthentication
	}
	credentialLength := int(binary.BigEndian.Uint16(authData[53:55]))
	if credentialLength == 0 || len(authData) < 55+credentialLength {
		return ErrAuthentication
	}
	credentialID := authData[55 : 55+credentialLength]
	decodedKeyID, err := decodeBase64(keyID)
	if err != nil || !bytes.Equal(credentialID, decodedKeyID) {
		return ErrAuthentication
	}
	return nil
}

func attestationCertificateNonce(certificate *x509.Certificate) ([]byte, error) {
	for _, extension := range certificate.Extensions {
		if !extension.Id.Equal(appleAttestationNonceOID) {
			continue
		}
		var value struct {
			Nonce []byte `asn1:"tag:1,explicit"`
		}
		if rest, err := asn1.Unmarshal(extension.Value, &value); err != nil || len(rest) != 0 || len(value.Nonce) != sha256.Size {
			return nil, ErrAuthentication
		}
		return value.Nonce, nil
	}
	return nil, errors.New("app attest nonce extension missing")
}
