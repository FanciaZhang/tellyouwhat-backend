package attestation

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

func TestAppleAttestationVerifierValidatesChainNonceAppIDEnvironmentAndKeyID(t *testing.T) {
	t.Parallel()

	fixture := makeAttestationFixture(t, "TEAMID.cn.tellyouwhat.healthapp", EnvironmentDevelopment)
	verifier := NewAppleAttestationVerifier(
		"TEAMID",
		"cn.tellyouwhat.healthapp",
		EnvironmentDevelopment,
		fixture.roots,
	)

	verified, err := verifier.Verify(fixture.keyID, fixture.object, fixture.clientDataHash)
	if err != nil {
		t.Fatalf("verify attestation: %v", err)
	}
	if len(verified.PublicKey) == 0 || string(verified.Receipt) != "receipt" {
		t.Fatalf("missing verified material: %+v", verified)
	}
}

func TestAppleAttestationVerifierRejectsWrongBundleBinding(t *testing.T) {
	t.Parallel()

	fixture := makeAttestationFixture(t, "TEAMID.cn.tellyouwhat.other", EnvironmentDevelopment)
	verifier := NewAppleAttestationVerifier(
		"TEAMID",
		"cn.tellyouwhat.healthapp",
		EnvironmentDevelopment,
		fixture.roots,
	)

	if _, err := verifier.Verify(fixture.keyID, fixture.object, fixture.clientDataHash); err == nil {
		t.Fatal("wrong App ID must be rejected")
	}
}

type attestationFixture struct {
	keyID          string
	object         []byte
	clientDataHash []byte
	roots          *x509.CertPool
}

func makeAttestationFixture(t *testing.T, appID string, environment Environment) attestationFixture {
	t.Helper()
	now := time.Now().UTC()
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test App Attest Root"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(rootDER)
	roots := x509.NewCertPool()
	roots.AddCert(root)

	attestedKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	publicX963, err := attestedKey.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	keyDigest := sha256.Sum256(publicX963)
	keyID := base64.StdEncoding.EncodeToString(keyDigest[:])
	clientDataHashArray := sha256.Sum256([]byte("registration challenge"))
	clientDataHash := clientDataHashArray[:]
	authData := make([]byte, 0, 37+16+2+len(keyDigest))
	rpIDHash := sha256.Sum256([]byte(appID))
	authData = append(authData, rpIDHash[:]...)
	authData = append(authData, byte(0x40))
	authData = append(authData, 0, 0, 0, 0)
	if environment == EnvironmentDevelopment {
		authData = append(authData, []byte("appattestdevelop")...)
	} else {
		authData = append(authData, append([]byte("appattest"), make([]byte, 7)...)...)
	}
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(keyDigest)))
	authData = append(authData, length...)
	authData = append(authData, keyDigest[:]...)
	nonceInput := append(append([]byte(nil), authData...), clientDataHash...)
	nonce := sha256.Sum256(nonceInput)
	extensionValue, err := asn1.Marshal(struct {
		Nonce []byte `asn1:"tag:1,explicit"`
	}{Nonce: nonce[:]})
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "Test App Attest Leaf"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtraExtensions: []pkix.Extension{{
			Id:    appleAttestationNonceOID,
			Value: extensionValue,
		}},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, root, &attestedKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	object, err := cbor.Marshal(map[string]any{
		"fmt":      "apple-appattest",
		"authData": authData,
		"attStmt": map[string]any{
			"x5c":     [][]byte{leafDER, rootDER},
			"receipt": []byte("receipt"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return attestationFixture{keyID: keyID, object: object, clientDataHash: clientDataHash, roots: roots}
}

