package attestation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/tellyouwhat/backend/internal/testutil/appattest"
)

func TestAssertionExtensionNamesShareValidationPolicy(t *testing.T) {
	t.Parallel()
	client := appattest.New(t, "TEAM.bundle", time.Now())
	hash := sha256.Sum256([]byte("bound request"))
	verifier := NewAppleAssertionVerifier("TEAM", "bundle")
	for _, tc := range []struct {
		name  string
		ext   map[string]any
		valid bool
	}{
		{"observed names", map[string]any{"apple_validation_category_01": []byte{4, 0, 0, 0}, "apple_bundle_version_01": "1057"}, true},
		{"documented names", map[string]any{"validationCategory": []byte{4, 0, 0, 0}, "bundleVersion": "1057"}, true},
		{"invalid documented category", map[string]any{"validationCategory": []byte{0, 0, 0, 0}}, false},
		{"invalid documented category type", map[string]any{"validationCategory": 4}, false},
		{"invalid documented version", map[string]any{"bundleVersion": " "}, false},
		{"invalid documented version type", map[string]any{"bundleVersion": 1057}, false},
		{"matching aliases", map[string]any{"validationCategory": []byte{4, 0, 0, 0}, "apple_validation_category_01": []byte{4, 0, 0, 0}, "bundleVersion": "1057", "apple_bundle_version_01": "1057"}, true},
		{"conflicting categories", map[string]any{"validationCategory": []byte{3, 0, 0, 0}, "apple_validation_category_01": []byte{4, 0, 0, 0}}, false},
		{"conflicting versions", map[string]any{"bundleVersion": "1057", "apple_bundle_version_01": "1200"}, false},
		{"unknown signed extension", map[string]any{"future_apple_field": []byte{1, 2, 3}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := client.AuthenticatorData(t, 1, 0xc0, tc.ext)
			_, err := verifier.VerifyAssertion(client.PublicKey, client.Sign(t, data, hash[:]), hash[:])
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestAssertionRejectsAmbiguousAndOversizedEnvelope(t *testing.T) {
	t.Parallel()
	client := appattest.New(t, "TEAM.bundle", time.Now())
	hash := sha256.Sum256([]byte("bound request"))
	valid := client.Sign(t, client.AppStoreData(t, 1), hash[:])
	var fields map[string]cbor.RawMessage
	if err := cbor.Unmarshal(valid, &fields); err != nil {
		t.Fatal(err)
	}
	key, err := cbor.Marshal("authenticatorData")
	if err != nil {
		t.Fatal(err)
	}
	sigKey, err := cbor.Marshal("signature")
	if err != nil {
		t.Fatal(err)
	}
	duplicate := []byte{0xa3}
	for _, part := range [][]byte{key, fields["authenticatorData"], key, fields["authenticatorData"], sigKey, fields["signature"]} {
		duplicate = append(duplicate, part...)
	}
	verifier := NewAppleAssertionVerifier("TEAM", "bundle")
	for _, data := range [][]byte{duplicate, append(append([]byte(nil), valid...), 0xa0), make([]byte, 32*1024)} {
		if _, err := verifier.VerifyAssertion(client.PublicKey, data, hash[:]); !errors.Is(err, ErrAuthentication) {
			t.Fatal("ambiguous or oversized assertion accepted")
		}
	}
}

func TestExpiredRegisteredEnrollmentPreservesIdentityWithVerifiedProof(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "used challenge", true: "expired challenge"}[expired], func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			client := appattest.New(t, "TEAM.bundle", now)
			keys := NewMemoryKeyStore()
			nonces := NewMemoryNonceStore()
			svc := NewEnrollmentService(EnrollmentConfig{AppID: "health", Environment: EnvironmentProduction}, nonces, keys, NewAppleAttestationVerifier("TEAM", "bundle", EnvironmentProduction, client.Roots), func() time.Time { return now })
			challenge, err := svc.IssueChallenge(ctx, client.KeyID)
			if err != nil {
				t.Fatal(err)
			}
			request := RegistrationRequest{KeyID: client.KeyID, Challenge: challenge.Value, Attestation: base64.StdEncoding.EncodeToString(client.Attestation(t, challenge.Value, "production"))}
			original, err := svc.Register(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := keys.Get(ctx, client.KeyID)
			if err != nil {
				t.Fatal(err)
			}
			stored.Counter = 41
			stored.TransactionID = "existing-transaction"
			keys.Put(stored)
			if expired {
				now = now.Add(6 * time.Minute)
			}
			recovered, err := svc.Register(ctx, request)
			if err != nil || recovered.DeviceID != original.DeviceID || recovered.TransactionID != stored.TransactionID {
				t.Fatalf("identity recovery failed: %+v %v", recovered, err)
			}
			after, err := keys.Get(ctx, client.KeyID)
			if err != nil || after.Counter != 41 {
				t.Fatal("recovery reset the assertion counter")
			}
			for _, tc := range []struct {
				name   string
				mutate func(*RegistrationRequest)
			}{
				{"wrong challenge", func(r *RegistrationRequest) { r.Challenge = "different-challenge" }},
				{"invalid proof", func(r *RegistrationRequest) { r.Attestation = base64.StdEncoding.EncodeToString([]byte("invalid")) }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					bad := request
					tc.mutate(&bad)
					if _, err := svc.Register(ctx, bad); err == nil {
						t.Fatal("invalid recovery proof accepted")
					}
				})
			}
			stored.PublicKey = []byte("different-public-key")
			keys.Put(stored)
			if _, err := svc.Register(ctx, request); err == nil {
				t.Fatal("proof for a different stored public key accepted")
			}
		})
	}
}

func TestRegistrationAttestationRejectsDuplicateMapKeys(t *testing.T) {
	t.Parallel()
	client := appattest.New(t, "TEAM.bundle", time.Now())
	challenge := "synthetic-challenge"
	object := client.Attestation(t, challenge, "production")
	var fields map[string]cbor.RawMessage
	if err := cbor.Unmarshal(object, &fields); err != nil {
		t.Fatal(err)
	}
	duplicate := []byte{0xa4}
	for _, name := range []string{"fmt", "authData", "attStmt", "fmt"} {
		key, err := cbor.Marshal(name)
		if err != nil {
			t.Fatal(err)
		}
		duplicate = append(duplicate, key...)
		duplicate = append(duplicate, fields[name]...)
	}
	hash := sha256.Sum256([]byte(challenge))
	verifier := NewAppleAttestationVerifier("TEAM", "bundle", EnvironmentProduction, client.Roots)
	if _, err := verifier.Verify(client.KeyID, duplicate, hash[:]); !errors.Is(err, ErrAuthentication) {
		t.Fatal("duplicate registration fields accepted")
	}
}

func TestExpiredRegistrationWithExpiredCertificateKeepsFreshEnrollmentFallback(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	client := appattest.New(t, "TEAM.bundle", now)
	verifier := NewAppleAttestationVerifier("TEAM", "bundle", EnvironmentProduction, client.Roots)
	verifier.now = func() time.Time { return now }
	svc := NewEnrollmentService(EnrollmentConfig{AppID: "health", Environment: EnvironmentProduction}, NewMemoryNonceStore(), NewMemoryKeyStore(), verifier, func() time.Time { return now })
	challenge, err := svc.IssueChallenge(context.Background(), client.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	request := RegistrationRequest{KeyID: client.KeyID, Challenge: challenge.Value, Attestation: base64.StdEncoding.EncodeToString(client.Attestation(t, challenge.Value, "production"))}
	if _, err := svc.Register(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	now = now.Add(25 * time.Hour)
	if _, err := svc.Register(context.Background(), request); !errors.Is(err, ErrAuthentication) || FailureStage(err) != "attestation_verification" {
		t.Fatalf("expired cached certificate lost the client's recovery contract: %v stage=%s", err, FailureStage(err))
	}
}

func FuzzAppleAssertionEnvelope(f *testing.F) {
	client := appattest.New(f, "TEAM.bundle", time.Now())
	hash := sha256.Sum256([]byte("bound request"))
	f.Add(client.Sign(f, client.AppStoreData(f, 1), hash[:]))
	f.Add(client.Sign(f, client.AuthenticatorData(f, 1, 0x01, nil), hash[:]))
	f.Add([]byte{0xa2, 0x61, 'x', 0x01, 0x61, 'x', 0x02})
	verifier := NewAppleAssertionVerifier("TEAM", "bundle")
	f.Fuzz(func(t *testing.T, data []byte) {
		_, err := verifier.VerifyAssertion(client.PublicKey, data, hash[:])
		if err != nil && !errors.Is(err, ErrAuthentication) {
			t.Fatalf("malformed proof changed the public error class: %v", err)
		}
	})
}
