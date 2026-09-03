// Created by OpenAI Codex on 2026-08-03.

package appstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestTransactionVerifierAcceptsAppleChainAndExpectedAppContract(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "original-transaction-1",
		TransactionID:         "transaction-2",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             "health.premium.subscription.monthly",
		Environment:           "Production",
		ExpiresDate:           now.Add(30 * 24 * time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	verifier := NewTransactionVerifier(VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductID:   "health.premium.subscription.monthly",
		Now:         func() time.Time { return now },
	})

	transaction, err := verifier.VerifyTransaction(fixture.signed)
	if err != nil {
		t.Fatalf("verify transaction: %v", err)
	}
	if transaction.OriginalTransactionID != "original-transaction-1" ||
		transaction.TransactionID != "transaction-2" {
		t.Fatalf("unexpected transaction: %+v", transaction)
	}
}

func TestTransactionVerifierAcceptsAnnualManagedSubscription(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "annual-original-transaction",
		TransactionID:         "annual-transaction",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             ManagedAnnualSubscriptionProductID,
		Environment:           "Production",
		ExpiresDate:           now.Add(365 * 24 * time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	verifier := NewTransactionVerifier(VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductIDs:  ManagedSubscriptionProductIDs(),
		Now:         func() time.Time { return now },
	})

	transaction, err := verifier.VerifyTransaction(fixture.signed)
	if err != nil {
		t.Fatalf("verify annual transaction: %v", err)
	}
	if transaction.ProductID != ManagedAnnualSubscriptionProductID {
		t.Fatalf("unexpected annual transaction: %+v", transaction)
	}
}

func TestTransactionVerifierRestrictsJournalSubscriptionPlans(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		productID string
		accepted  bool
	}{
		{"journal.ai.subscription.monthly", true},
		{"journal.ai.subscription.annual", true},
		{"health.premium.subscription.annual", false},
		{"journal.ai.subscription.quarterly", false},
	} {
		t.Run(test.productID, func(t *testing.T) {
			fixture := newSignedTransactionFixture(t, now, transactionPayload{
				OriginalTransactionID: "journal-original-transaction",
				TransactionID:         "journal-transaction",
				BundleID:              "cn.tellyouwhat.journalapp",
				ProductID:             test.productID,
				Environment:           "Production",
				ExpiresDate:           now.Add(365 * 24 * time.Hour).UnixMilli(),
				SignedDate:            now.UnixMilli(),
			})
			verifier := NewTransactionVerifier(VerifierConfig{
				Roots: fixture.roots, BundleID: "cn.tellyouwhat.journalapp",
				AppAppleID: 6808104188, Environment: "Production",
				ProductIDs: []string{"journal.ai.subscription.monthly", "journal.ai.subscription.annual"},
				Now:        func() time.Time { return now },
			})
			transaction, err := verifier.VerifyTransaction(fixture.signed)
			if test.accepted {
				if err != nil || transaction.ProductID != test.productID {
					t.Fatalf("Journal plan rejected: product=%s error=%v", transaction.ProductID, err)
				}
			} else if !errors.Is(err, ErrInvalidSignedData) {
				t.Fatalf("unconfigured Journal plan accepted: %v", err)
			}
		})
	}
}

func TestTransactionVerifierRejectsProductOutsideManagedSubscriptionAllowlist(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "unrelated-original-transaction",
		TransactionID:         "unrelated-transaction",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             "health.premium.subscription.quarterly",
		Environment:           "Production",
		ExpiresDate:           now.Add(90 * 24 * time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	verifier := NewTransactionVerifier(VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductIDs:  ManagedSubscriptionProductIDs(),
		Now:         func() time.Time { return now },
	})

	if _, err := verifier.VerifyTransaction(fixture.signed); !errors.Is(err, ErrInvalidSignedData) {
		t.Fatalf("unexpected product accepted: %v", err)
	}
}

func TestTransactionVerifierAcceptsOfferCodeMetadataWithinExpectedProduct(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "offer-original-transaction",
		TransactionID:         "offer-transaction",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             ManagedSubscriptionProductID,
		Environment:           "Production",
		OfferIdentifier:       "friends-launch-month",
		OfferType:             3,
		ExpiresDate:           now.Add(30 * 24 * time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	verifier := NewTransactionVerifier(VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductID:   ManagedSubscriptionProductID,
		Now:         func() time.Time { return now },
	})

	transaction, err := verifier.VerifyTransaction(fixture.signed)
	if err != nil {
		t.Fatalf("verify offer code transaction: %v", err)
	}
	if transaction.OfferIdentifier != "friends-launch-month" || transaction.OfferType != 3 ||
		transaction.ProductID != ManagedSubscriptionProductID {
		t.Fatalf("unexpected offer code transaction: %+v", transaction)
	}
}

func TestTransactionVerifierRejectsTamperingAndWrongAppIdentity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "original-transaction-1",
		TransactionID:         "transaction-2",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             "health.premium.subscription.monthly",
		Environment:           "Production",
		ExpiresDate:           now.Add(30 * 24 * time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	validConfig := VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductID:   "health.premium.subscription.monthly",
		Now:         func() time.Time { return now },
	}
	segments := strings.Split(fixture.signed, ".")
	signature, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil {
		t.Fatalf("decode fixture signature: %v", err)
	}
	signature[0] ^= 0xff
	segments[2] = base64.RawURLEncoding.EncodeToString(signature)
	tampered := strings.Join(segments, ".")
	if _, err := NewTransactionVerifier(validConfig).VerifyTransaction(tampered); !errors.Is(err, ErrInvalidSignedData) {
		t.Fatalf("tampered signature accepted: %v", err)
	}
	wrongAppConfig := validConfig
	wrongAppConfig.BundleID = "com.example.stolen"
	if _, err := NewTransactionVerifier(wrongAppConfig).VerifyTransaction(fixture.signed); !errors.Is(err, ErrInvalidSignedData) {
		t.Fatalf("wrong app identity accepted: %v", err)
	}
}

func TestSubscriptionResolverConfirmsCurrentActiveStatusWithAppleAPI(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "original-transaction-1",
		TransactionID:         "transaction-2",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             "health.premium.subscription.monthly",
		Environment:           "Production",
		ExpiresDate:           now.Add(30 * 24 * time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	verifier := NewTransactionVerifier(VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductID:   "health.premium.subscription.monthly",
		Now:         func() time.Time { return now },
	})
	statusClient := &fakeStatusClient{statuses: []SubscriptionStatus{{
		Status:            1,
		SignedTransaction: fixture.signed,
	}}}
	resolver := NewSubscriptionResolver(verifier, statusClient, func() time.Time { return now })

	state, err := resolver.Resolve(context.Background(), fixture.signed)
	if err != nil {
		t.Fatalf("resolve subscription: %v", err)
	}
	if statusClient.transactionID != "transaction-2" {
		t.Fatalf("status lookup used unexpected transaction: %q", statusClient.transactionID)
	}
	if state.OriginalTransactionID != "original-transaction-1" ||
		!state.ExpiresAt.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("unexpected subscription state: %+v", state)
	}
}

func TestSubscriptionResolverAcceptsMonthlyToAnnualCrossgrade(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "crossgrade-original",
		TransactionID:         "monthly-transaction",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             ManagedMonthlySubscriptionProductID,
		Environment:           "Production",
		ExpiresDate:           now.Add(30 * 24 * time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	annualExpiry := now.Add(365 * 24 * time.Hour)
	annualTransaction := fixture.signPayload(t, transactionPayload{
		OriginalTransactionID: "crossgrade-original",
		TransactionID:         "annual-transaction",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             ManagedAnnualSubscriptionProductID,
		Environment:           "Production",
		ExpiresDate:           annualExpiry.UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	verifier := NewTransactionVerifier(VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductIDs:  ManagedSubscriptionProductIDs(),
		Now:         func() time.Time { return now },
	})
	resolver := NewSubscriptionResolver(verifier, &fakeStatusClient{statuses: []SubscriptionStatus{{
		Status:            statusActive,
		SignedTransaction: annualTransaction,
	}}}, func() time.Time { return now })

	state, err := resolver.Resolve(context.Background(), fixture.signed)
	if err != nil {
		t.Fatalf("resolve crossgrade: %v", err)
	}
	if state.TransactionID != "annual-transaction" || !state.ExpiresAt.Equal(annualExpiry) {
		t.Fatalf("unexpected crossgrade state: %+v", state)
	}
}

func TestSubscriptionResolverHonorsVerifiedBillingGracePeriod(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "original-transaction-1",
		TransactionID:         "transaction-2",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             "health.premium.subscription.monthly",
		Environment:           "Production",
		ExpiresDate:           now.Add(-time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	graceExpiry := now.Add(3 * 24 * time.Hour)
	signedRenewal := fixture.signPayload(t, renewalPayload{
		OriginalTransactionID:  "original-transaction-1",
		AutoRenewProductID:     "health.premium.subscription.monthly",
		Environment:            "Production",
		GracePeriodExpiresDate: graceExpiry.UnixMilli(),
		SignedDate:             now.UnixMilli(),
	})
	verifier := NewTransactionVerifier(VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductID:   "health.premium.subscription.monthly",
		Now:         func() time.Time { return now },
	})
	resolver := NewSubscriptionResolver(verifier, &fakeStatusClient{statuses: []SubscriptionStatus{{
		Status:            4,
		SignedTransaction: fixture.signed,
		SignedRenewal:     signedRenewal,
	}}}, func() time.Time { return now })

	state, err := resolver.Resolve(context.Background(), fixture.signed)
	if err != nil {
		t.Fatalf("resolve grace subscription: %v", err)
	}
	if !state.ExpiresAt.Equal(graceExpiry) {
		t.Fatalf("unexpected grace expiry: %s", state.ExpiresAt)
	}
}

func TestSubscriptionResolverRejectsInactiveCurrentState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "original-transaction-1",
		TransactionID:         "transaction-2",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             ManagedSubscriptionProductID,
		Environment:           "Production",
		ExpiresDate:           now.Add(-time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	verifier := NewTransactionVerifier(VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductID:   ManagedSubscriptionProductID,
		Now:         func() time.Time { return now },
	})
	resolver := NewSubscriptionResolver(verifier, &fakeStatusClient{}, func() time.Time { return now })

	_, err := resolver.Resolve(context.Background(), fixture.signed)
	if !errors.Is(err, ErrSubscriptionInactive) {
		t.Fatalf("expected inactive subscription, got %v", err)
	}
}

func TestNotificationProcessorHandlesRenewalAndOfferCodeNotifications(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "original-transaction-1",
		TransactionID:         "transaction-2",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             "health.premium.subscription.monthly",
		Environment:           "Production",
		ExpiresDate:           now.Add(30 * 24 * time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	verifier := NewTransactionVerifier(VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductID:   "health.premium.subscription.monthly",
		Now:         func() time.Time { return now },
	})
	resolver := NewSubscriptionResolver(verifier, &fakeStatusClient{statuses: []SubscriptionStatus{{
		Status:            1,
		SignedTransaction: fixture.signed,
	}}}, func() time.Time { return now })
	processor := NewNotificationProcessor(verifier, resolver)

	for _, testCase := range []struct {
		name             string
		notificationType string
		notificationUUID string
	}{
		{name: "renewal", notificationType: "DID_RENEW", notificationUUID: "renewal-notification-uuid"},
		{name: "offer code redemption", notificationType: "OFFER_REDEEMED", notificationUUID: "offer-notification-uuid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			signedNotification := fixture.signPayload(t, notificationPayload{
				NotificationType: testCase.notificationType,
				NotificationUUID: testCase.notificationUUID,
				Version:          "2.0",
				SignedDate:       now.UnixMilli(),
				Data: notificationData{
					AppAppleID:            1234567890,
					BundleID:              "cn.tellyouwhat.healthapp",
					Environment:           "Production",
					Status:                1,
					SignedTransactionInfo: fixture.signed,
				},
			})

			result, err := processor.Process(context.Background(), signedNotification)
			if err != nil {
				t.Fatalf("process notification: %v", err)
			}
			if result.NotificationUUID != testCase.notificationUUID ||
				result.OriginalTransactionID != "original-transaction-1" || !result.Active {
				t.Fatalf("unexpected notification result: %+v", result)
			}
		})
	}
}

func TestNotificationProcessorAcceptsAppleTestNotificationWithoutTransaction(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)
	fixture := newSignedTransactionFixture(t, now, transactionPayload{
		OriginalTransactionID: "fixture-original",
		TransactionID:         "fixture-transaction",
		BundleID:              "cn.tellyouwhat.healthapp",
		ProductID:             "health.premium.subscription.monthly",
		Environment:           "Production",
		ExpiresDate:           now.Add(time.Hour).UnixMilli(),
		SignedDate:            now.UnixMilli(),
	})
	signedNotification := fixture.signPayload(t, notificationPayload{
		NotificationType: "TEST",
		NotificationUUID: "test-notification-uuid",
		Version:          "2.0",
		SignedDate:       now.UnixMilli(),
		Data: notificationData{
			AppAppleID:  1234567890,
			BundleID:    "cn.tellyouwhat.healthapp",
			Environment: "Production",
		},
	})
	verifier := NewTransactionVerifier(VerifierConfig{
		Roots:       fixture.roots,
		BundleID:    "cn.tellyouwhat.healthapp",
		AppAppleID:  1234567890,
		Environment: "Production",
		ProductID:   "health.premium.subscription.monthly",
		Now:         func() time.Time { return now },
	})
	processor := NewNotificationProcessor(
		verifier,
		NewSubscriptionResolver(verifier, &fakeStatusClient{}, func() time.Time { return now }),
	)

	result, err := processor.Process(context.Background(), signedNotification)
	if err != nil {
		t.Fatalf("process test notification: %v", err)
	}
	if result.NotificationUUID != "test-notification-uuid" ||
		result.OriginalTransactionID != "test:test-notification-uuid" || result.Active {
		t.Fatalf("unexpected test notification result: %+v", result)
	}
}

type fakeStatusClient struct {
	transactionID string
	statuses      []SubscriptionStatus
	err           error
}

func (client *fakeStatusClient) CurrentTransactions(
	_ context.Context,
	transactionID string,
) ([]SubscriptionStatus, error) {
	client.transactionID = transactionID
	return client.statuses, client.err
}

type signedTransactionFixture struct {
	signed        string
	roots         *x509.CertPool
	headerSegment string
	leafKey       *ecdsa.PrivateKey
}

func (fixture signedTransactionFixture) signPayload(t *testing.T, payload any) string {
	t.Helper()
	payloadData, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal signed payload: %v", err)
	}
	payloadSegment := base64.RawURLEncoding.EncodeToString(payloadData)
	signingInput := fixture.headerSegment + "." + payloadSegment
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, fixture.leafKey, digest[:])
	if err != nil {
		t.Fatalf("sign JWS: %v", err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func newSignedTransactionFixture(
	t *testing.T,
	now time.Time,
	payload transactionPayload,
) signedTransactionFixture {
	t.Helper()
	rootKey := newTestECKey(t)
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Apple Root"},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	rootDER := createTestCertificate(t, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	root := parseTestCertificate(t, rootDER)

	intermediateKey := newTestECKey(t)
	intermediateTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "Test WWDR"},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(180 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtraExtensions: []pkix.Extension{{
			Id:    oidWWDRIntermediate,
			Value: []byte{0x05, 0x00},
		}},
	}
	intermediateDER := createTestCertificate(
		t,
		intermediateTemplate,
		root,
		&intermediateKey.PublicKey,
		rootKey,
	)
	intermediate := parseTestCertificate(t, intermediateDER)

	leafKey := newTestECKey(t)
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "Test Receipt Signer"},
		NotBefore:    now.Add(-24 * time.Hour),
		NotAfter:     now.Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtraExtensions: []pkix.Extension{{
			Id:    oidReceiptSigner,
			Value: []byte{0x05, 0x00},
		}},
	}
	leafDER := createTestCertificate(t, leafTemplate, intermediate, &leafKey.PublicKey, intermediateKey)

	headerData, err := json.Marshal(map[string]any{
		"alg": "ES256",
		"x5c": []string{
			base64.StdEncoding.EncodeToString(leafDER),
			base64.StdEncoding.EncodeToString(intermediateDER),
			base64.StdEncoding.EncodeToString(rootDER),
		},
	})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	headerSegment := base64.RawURLEncoding.EncodeToString(headerData)

	roots := x509.NewCertPool()
	roots.AddCert(root)
	fixture := signedTransactionFixture{
		roots:         roots,
		headerSegment: headerSegment,
		leafKey:       leafKey,
	}
	fixture.signed = fixture.signPayload(t, payload)
	return fixture
}

func newTestECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func createTestCertificate(
	t *testing.T,
	template *x509.Certificate,
	parent *x509.Certificate,
	publicKey *ecdsa.PublicKey,
	signer *ecdsa.PrivateKey,
) []byte {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, publicKey, signer)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return der
}

func parseTestCertificate(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return certificate
}
