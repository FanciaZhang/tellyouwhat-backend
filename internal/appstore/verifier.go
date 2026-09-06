// Created by OpenAI Codex on 2026-08-03.

package appstore

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"
)

const (
	ManagedMonthlySubscriptionProductID = "health.premium.subscription.monthly"
	ManagedAnnualSubscriptionProductID  = "health.premium.subscription.annual"
	ManagedSubscriptionProductID        = ManagedMonthlySubscriptionProductID
	expectedJWSAlgorithm                = "ES256"
	expectedJWSSegments                 = 3
	expectedJWSChainLength              = 3
	maximumSignedDataBytes              = 1 << 20
)

func ManagedSubscriptionProductIDs() []string {
	return []string{
		ManagedMonthlySubscriptionProductID,
		ManagedAnnualSubscriptionProductID,
	}
}

var (
	ErrInvalidSignedData = errors.New("invalid App Store signed data")
	oidWWDRIntermediate  = []int{1, 2, 840, 113635, 100, 6, 2, 1}
	oidReceiptSigner     = []int{1, 2, 840, 113635, 100, 6, 11, 1}
)

type VerifierConfig struct {
	Roots       *x509.CertPool
	BundleID    string
	AppAppleID  int64
	Environment string
	ProductID   string
	ProductIDs  []string
	Now         func() time.Time
}

type Transaction struct {
	OriginalTransactionID string
	TransactionID         string
	BundleID              string
	ProductID             string
	Environment           string
	OfferIdentifier       string
	OfferType             int32
	ExpiresAt             time.Time
	StartedAt             time.Time
	SignedAt              time.Time
	RevokedAt             *time.Time
	IsUpgraded            bool
}

type Renewal struct {
	OriginalTransactionID string
	AutoRenewProductID    string
	Environment           string
	GracePeriodExpiresAt  time.Time
	SignedAt              time.Time
}

type TransactionVerifier struct {
	config            VerifierConfig
	allowedProductIDs map[string]struct{}
}

type transactionPayload struct {
	OriginalTransactionID string `json:"originalTransactionId"`
	TransactionID         string `json:"transactionId"`
	BundleID              string `json:"bundleId"`
	ProductID             string `json:"productId"`
	Environment           string `json:"environment"`
	OfferIdentifier       string `json:"offerIdentifier"`
	OfferType             int32  `json:"offerType"`
	ExpiresDate           int64  `json:"expiresDate"`
	OriginalPurchaseDate  int64  `json:"originalPurchaseDate"`
	SignedDate            int64  `json:"signedDate"`
	RevocationDate        *int64 `json:"revocationDate"`
	IsUpgraded            bool   `json:"isUpgraded"`
}

type renewalPayload struct {
	OriginalTransactionID  string `json:"originalTransactionId"`
	AutoRenewProductID     string `json:"autoRenewProductId"`
	Environment            string `json:"environment"`
	GracePeriodExpiresDate int64  `json:"gracePeriodExpiresDate"`
	SignedDate             int64  `json:"signedDate"`
}

type jwsHeader struct {
	Algorithm string   `json:"alg"`
	Chain     []string `json:"x5c"`
}

func NewTransactionVerifier(config VerifierConfig) *TransactionVerifier {
	if config.Now == nil {
		config.Now = time.Now
	}
	allowedProductIDs := make(map[string]struct{}, len(config.ProductIDs))
	if config.ProductID != "" {
		allowedProductIDs[config.ProductID] = struct{}{}
	}
	for _, productID := range config.ProductIDs {
		if productID != "" {
			allowedProductIDs[productID] = struct{}{}
		}
	}
	return &TransactionVerifier{
		config:            config,
		allowedProductIDs: allowedProductIDs,
	}
}

func (verifier *TransactionVerifier) VerifyTransaction(signedData string) (Transaction, error) {
	if verifier == nil || verifier.config.Roots == nil || verifier.config.BundleID == "" ||
		len(verifier.allowedProductIDs) == 0 || len(signedData) == 0 || len(signedData) > maximumSignedDataBytes {
		return Transaction{}, ErrInvalidSignedData
	}
	header, payload, segments, err := decodeTransactionJWS(signedData)
	if err != nil {
		return Transaction{}, ErrInvalidSignedData
	}
	certificates, err := verifier.verifyCertificateChain(header, payload.SignedDate)
	if err != nil {
		return Transaction{}, ErrInvalidSignedData
	}
	if err := verifyJWSSignature(certificates[0], segments); err != nil {
		return Transaction{}, ErrInvalidSignedData
	}
	if !verifier.matchesTransaction(payload) {
		return Transaction{}, ErrInvalidSignedData
	}
	return normalizedTransaction(payload), nil
}

func (verifier *TransactionVerifier) VerifyRenewal(signedData string) (Renewal, error) {
	if verifier == nil || verifier.config.Roots == nil || len(verifier.allowedProductIDs) == 0 ||
		len(signedData) == 0 || len(signedData) > maximumSignedDataBytes {
		return Renewal{}, ErrInvalidSignedData
	}
	header, payload, segments, err := decodeRenewalJWS(signedData)
	if err != nil {
		return Renewal{}, ErrInvalidSignedData
	}
	certificates, err := verifier.verifyCertificateChain(header, payload.SignedDate)
	if err != nil {
		return Renewal{}, ErrInvalidSignedData
	}
	if err := verifyJWSSignature(certificates[0], segments); err != nil {
		return Renewal{}, ErrInvalidSignedData
	}
	if payload.OriginalTransactionID == "" || !verifier.acceptsProductID(payload.AutoRenewProductID) ||
		!strings.EqualFold(payload.Environment, verifier.config.Environment) || payload.SignedDate <= 0 ||
		time.UnixMilli(payload.SignedDate).After(verifier.config.Now().Add(5*time.Minute)) {
		return Renewal{}, ErrInvalidSignedData
	}
	return Renewal{
		OriginalTransactionID: payload.OriginalTransactionID,
		AutoRenewProductID:    payload.AutoRenewProductID,
		Environment:           payload.Environment,
		GracePeriodExpiresAt:  time.UnixMilli(payload.GracePeriodExpiresDate),
		SignedAt:              time.UnixMilli(payload.SignedDate),
	}, nil
}

func decodeTransactionJWS(signedData string) (jwsHeader, transactionPayload, []string, error) {
	segments := strings.Split(signedData, ".")
	if len(segments) != expectedJWSSegments {
		return jwsHeader{}, transactionPayload{}, nil, ErrInvalidSignedData
	}
	headerData, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return jwsHeader{}, transactionPayload{}, nil, err
	}
	payloadData, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return jwsHeader{}, transactionPayload{}, nil, err
	}
	var header jwsHeader
	if err := json.Unmarshal(headerData, &header); err != nil {
		return jwsHeader{}, transactionPayload{}, nil, err
	}
	var payload transactionPayload
	if err := json.Unmarshal(payloadData, &payload); err != nil {
		return jwsHeader{}, transactionPayload{}, nil, err
	}
	return header, payload, segments, nil
}

func decodeRenewalJWS(signedData string) (jwsHeader, renewalPayload, []string, error) {
	segments := strings.Split(signedData, ".")
	if len(segments) != expectedJWSSegments {
		return jwsHeader{}, renewalPayload{}, nil, ErrInvalidSignedData
	}
	headerData, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return jwsHeader{}, renewalPayload{}, nil, err
	}
	payloadData, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return jwsHeader{}, renewalPayload{}, nil, err
	}
	var header jwsHeader
	if err := json.Unmarshal(headerData, &header); err != nil {
		return jwsHeader{}, renewalPayload{}, nil, err
	}
	var payload renewalPayload
	if err := json.Unmarshal(payloadData, &payload); err != nil {
		return jwsHeader{}, renewalPayload{}, nil, err
	}
	return header, payload, segments, nil
}

func (verifier *TransactionVerifier) verifyCertificateChain(
	header jwsHeader,
	signedDate int64,
) ([]*x509.Certificate, error) {
	if header.Algorithm != expectedJWSAlgorithm || len(header.Chain) != expectedJWSChainLength || signedDate <= 0 {
		return nil, ErrInvalidSignedData
	}
	certificates := make([]*x509.Certificate, 0, expectedJWSChainLength)
	for _, encoded := range header.Chain {
		der, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, err
		}
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, certificate)
	}
	if !hasExtension(certificates[0], oidReceiptSigner) ||
		!hasExtension(certificates[1], oidWWDRIntermediate) {
		return nil, ErrInvalidSignedData
	}
	intermediates := x509.NewCertPool()
	intermediates.AddCert(certificates[1])
	chains, err := certificates[0].Verify(x509.VerifyOptions{
		Roots:         verifier.config.Roots,
		Intermediates: intermediates,
		CurrentTime:   verifier.config.Now(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		return nil, err
	}
	for _, chain := range chains {
		if len(chain) == expectedJWSChainLength && bytes.Equal(chain[2].Raw, certificates[2].Raw) {
			return certificates, nil
		}
	}
	return nil, ErrInvalidSignedData
}

func (verifier *TransactionVerifier) matchesTransaction(payload transactionPayload) bool {
	if payload.BundleID != verifier.config.BundleID || !verifier.acceptsProductID(payload.ProductID) ||
		payload.OriginalTransactionID == "" || payload.TransactionID == "" || payload.ExpiresDate <= 0 {
		return false
	}
	if !strings.EqualFold(payload.Environment, verifier.config.Environment) {
		return false
	}
	signedAt := time.UnixMilli(payload.SignedDate)
	return !signedAt.After(verifier.config.Now().Add(5 * time.Minute))
}

func (verifier *TransactionVerifier) acceptsProductID(productID string) bool {
	if verifier == nil {
		return false
	}
	_, allowed := verifier.allowedProductIDs[productID]
	return allowed
}

func normalizedTransaction(payload transactionPayload) Transaction {
	var revokedAt *time.Time
	if payload.RevocationDate != nil {
		value := time.UnixMilli(*payload.RevocationDate)
		revokedAt = &value
	}
	return Transaction{
		OriginalTransactionID: payload.OriginalTransactionID,
		TransactionID:         payload.TransactionID,
		BundleID:              payload.BundleID,
		ProductID:             payload.ProductID,
		Environment:           payload.Environment,
		OfferIdentifier:       payload.OfferIdentifier,
		OfferType:             payload.OfferType,
		ExpiresAt:             time.UnixMilli(payload.ExpiresDate),
		StartedAt:             verifiedPurchaseStart(payload.OriginalPurchaseDate),
		SignedAt:              time.UnixMilli(payload.SignedDate),
		RevokedAt:             revokedAt,
		IsUpgraded:            payload.IsUpgraded,
	}
}

func verifyJWSSignature(certificate *x509.Certificate, segments []string) error {
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return ErrInvalidSignedData
	}
	signature, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil || len(signature) != 64 {
		return ErrInvalidSignedData
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(publicKey, digest[:], r, s) {
		return ErrInvalidSignedData
	}
	return nil
}

func hasExtension(certificate *x509.Certificate, oid []int) bool {
	for _, extension := range certificate.Extensions {
		if extension.Id.Equal(oid) {
			return true
		}
	}
	return false
}

func verifiedPurchaseStart(milliseconds int64) time.Time {
	if milliseconds <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(milliseconds)
}
