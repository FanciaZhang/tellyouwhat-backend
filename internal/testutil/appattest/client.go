// Package appattest supplies synthetic Apple-format proofs for integration tests.
// It uses a private test CA and contains no production keys or captured proofs.
package appattest

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
	"encoding/hex"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

type Client struct {
	KeyID     string
	PublicKey []byte
	Roots     *x509.CertPool
	key       *ecdsa.PrivateKey
	issuer    *x509.Certificate
	issuerKey *ecdsa.PrivateKey
	rp        [32]byte
	now       time.Time
}

func must[T any](t testing.TB, value T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func New(t testing.TB, appID string, now time.Time) *Client {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	key = must(t, key, err)
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	issuerKey = must(t, issuerKey, err)
	issuerTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Synthetic App Attest CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	issuerDER, err := x509.CreateCertificate(rand.Reader, issuerTemplate, issuerTemplate, &issuerKey.PublicKey, issuerKey)
	issuerDER = must(t, issuerDER, err)
	issuer, err := x509.ParseCertificate(issuerDER)
	issuer = must(t, issuer, err)
	roots := x509.NewCertPool()
	roots.AddCert(issuer)
	publicX963, err := key.PublicKey.Bytes()
	publicX963 = must(t, publicX963, err)
	keyHash := sha256.Sum256(publicX963)
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return &Client{KeyID: base64.StdEncoding.EncodeToString(keyHash[:]), PublicKey: must(t, publicDER, err),
		Roots: roots, key: key, issuer: issuer, issuerKey: issuerKey, rp: sha256.Sum256([]byte(appID)), now: now}
}

func (client *Client) Attestation(t testing.TB, challenge, environment string) []byte {
	t.Helper()
	keyID, err := base64.StdEncoding.DecodeString(client.KeyID)
	keyID = must(t, keyID, err)
	data := append([]byte(nil), client.rp[:]...)
	data = append(data, 0x40, 0, 0, 0, 0)
	if environment == "development" {
		data = append(data, []byte("appattestdevelop")...)
	} else {
		data = append(data, []byte("appattest\x00\x00\x00\x00\x00\x00\x00")...)
	}
	data = append(data, 0, 32)
	data = append(data, keyID...)
	credentialKey, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1,
		-2: client.key.X.FillBytes(make([]byte, 32)), -3: client.key.Y.FillBytes(make([]byte, 32))})
	data = append(data, must(t, credentialKey, err)...)
	hash := sha256.Sum256([]byte(challenge))
	nonce := sha256.Sum256(append(append([]byte(nil), data...), hash[:]...))
	extension, err := asn1.Marshal(struct {
		Nonce []byte `asn1:"tag:1,explicit"`
	}{nonce[:]})
	extension = must(t, extension, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: client.now.Add(-time.Hour), NotAfter: client.now.Add(24 * time.Hour),
		KeyUsage:        x509.KeyUsageDigitalSignature,
		ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}, Value: extension}}}
	cert, err := x509.CreateCertificate(rand.Reader, leaf, client.issuer, &client.key.PublicKey, client.issuerKey)
	cert = must(t, cert, err)
	object, err := cbor.Marshal(map[string]any{"fmt": "apple-appattest", "authData": data,
		"attStmt": map[string]any{"x5c": [][]byte{cert, client.issuer.Raw}, "receipt": []byte("synthetic-receipt")}})
	return must(t, object, err)
}

// Assert reproduces the shipped Swift client's newline binding independently
// of the server's RequestBindingDigest. data is signed exactly as supplied.
func (client *Client) Assert(t testing.TB, request *http.Request, body []byte, data []byte) {
	t.Helper()
	bodyHash := sha256.Sum256(body)
	lines := []string{strings.ToUpper(request.Method), request.URL.EscapedPath(),
		strings.ToLower(request.Header.Get("X-Tellyouwhat-Request-ID")), request.Header.Get("X-Tellyouwhat-Nonce"),
		request.Header.Get("X-Tellyouwhat-Timestamp"), hex.EncodeToString(bodyHash[:])}
	hash := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	request.Header.Set("X-Tellyouwhat-Key-ID", client.KeyID)
	request.Header.Set("X-Tellyouwhat-Assertion", base64.StdEncoding.EncodeToString(client.Sign(t, data, hash[:])))
}

func (client *Client) Sign(t testing.TB, data, hash []byte) []byte {
	t.Helper()
	nonce := sha256.Sum256(append(append([]byte(nil), data...), hash[:]...))
	digest := sha256.Sum256(nonce[:])
	sig, err := ecdsa.SignASN1(rand.Reader, client.key, digest[:])
	sig = must(t, sig, err)
	assertion, err := cbor.Marshal(map[string]any{"authenticatorData": data, "signature": sig})
	return must(t, assertion, err)
}

// AppStoreData preserves the independently established 102-byte, 0xc0 wire
// layout. Only the RP hash, counter and synthetic four-character build differ.
func (client *Client) AppStoreData(t testing.TB, counter uint32) []byte {
	t.Helper()
	ext, err := hex.DecodeString("a2781c6170706c655f76616c69646174696f6e5f63617465676f72795f30314404000000776170706c655f62756e646c655f76657273696f6e5f30316431303537")
	return append(client.AuthenticatorData(t, counter, 0xc0, nil), must(t, ext, err)...)
}

func (client *Client) AuthenticatorData(t testing.TB, counter uint32, flags byte, extensions map[string]any) []byte {
	t.Helper()
	data := make([]byte, 37)
	copy(data, client.rp[:])
	data[32] = flags
	binary.BigEndian.PutUint32(data[33:], counter)
	if extensions != nil {
		encoded, err := cbor.Marshal(extensions)
		data = append(data, must(t, encoded, err)...)
	}
	return data
}
