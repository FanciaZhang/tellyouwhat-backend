package albums

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/appstore"
)

type albumReceiptFixture struct {
	roots   *x509.CertPool
	rootDER []byte
	key     *ecdsa.PrivateKey
	header  string
	now     time.Time
}

func albumTestECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Real certificate-chain and ES256 verification; the root is a test authority,
// not Apple, and never enters a deployment or a production certificate file.
func newAlbumReceiptFixture(t *testing.T, now time.Time) albumReceiptFixture {
	t.Helper()
	rootKey, intermediateKey, leafKey := albumTestECKey(t), albumTestECKey(t), albumTestECKey(t)
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Album test root"}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.AddDate(2, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	makeCert := func(template, parent *x509.Certificate, key *ecdsa.PrivateKey, signer *ecdsa.PrivateKey) ([]byte, *x509.Certificate) {
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return der, cert
	}
	rootDER, rootCert := makeCert(root, root, rootKey, rootKey)
	intermediate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Album test WWDR"}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtraExtensions: []pkix.Extension{{Id: []int{1, 2, 840, 113635, 100, 6, 2, 1}, Value: []byte{5, 0}}}}
	intermediateDER, intermediateCert := makeCert(intermediate, rootCert, intermediateKey, rootKey)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "Album test receipt signer"}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.AddDate(0, 3, 0), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtraExtensions: []pkix.Extension{{Id: []int{1, 2, 840, 113635, 100, 6, 11, 1}, Value: []byte{5, 0}}}}
	leafDER, _ := makeCert(leaf, intermediateCert, leafKey, intermediateKey)
	header, err := json.Marshal(map[string]any{"alg": "ES256", "x5c": []string{base64.StdEncoding.EncodeToString(leafDER), base64.StdEncoding.EncodeToString(intermediateDER), base64.StdEncoding.EncodeToString(rootDER)}})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(rootCert)
	return albumReceiptFixture{roots: roots, rootDER: rootDER, key: leafKey, header: base64.RawURLEncoding.EncodeToString(header), now: now}
}

func (f albumReceiptFixture) sign(t *testing.T, payload map[string]any) string {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	message := f.header + "." + base64.RawURLEncoding.EncodeToString(data)
	digest := sha256.Sum256([]byte(message))
	r, s, err := ecdsa.Sign(rand.Reader, f.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	proof := make([]byte, 64)
	r.FillBytes(proof[:32])
	s.FillBytes(proof[32:])
	return message + "." + base64.RawURLEncoding.EncodeToString(proof)
}
func (f albumReceiptFixture) payload(owner string) map[string]any {
	return map[string]any{"appAccountToken": owner, "inAppOwnershipType": "PURCHASED", "bundleId": albumAppleAudience, "environment": "Sandbox", "productId": Cloud50GBAnnualProduct,
		"originalTransactionId": "1000000000000001", "transactionId": "1000000000000002", "originalPurchaseDate": f.now.Add(-time.Hour).UnixMilli(), "purchaseDate": f.now.Add(-time.Hour).UnixMilli(),
		"expiresDate": f.now.AddDate(1, 0, 0).UnixMilli(), "signedDate": f.now.UnixMilli()}
}
func (f albumReceiptFixture) verifier() *appstore.TransactionVerifier {
	return appstore.NewTransactionVerifier(appstore.VerifierConfig{Roots: f.roots, BundleID: albumAppleAudience, Environment: "Sandbox", ProductIDs: []string{Cloud50GBAnnualProduct, Cloud500GBAnnualProduct}, Now: func() time.Time { return f.now }})
}
