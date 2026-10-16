package attestation

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/testutil/appattest"
)

func TestRestrictedEnrollmentWithProductionProof(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	client := appattest.New(t, "TEAM.bundle.dev", now)
	keys := NewMemoryKeyStore()
	service := NewEnrollmentService(EnrollmentConfig{
		AppID: "health", Environment: EnvironmentProduction,
		RequireActivationSecret: true, DevelopmentSecret: "synthetic-secret",
		AllowedBuilds: map[string]struct{}{"100": {}},
	}, NewMemoryNonceStore(), keys,
		NewAppleAttestationVerifier("TEAM", "bundle.dev", EnvironmentProduction, client.Roots), func() time.Time { return now })
	challenge, err := service.IssueChallenge(ctx, client.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	request := RegistrationRequest{KeyID: client.KeyID, Challenge: challenge.Value, Build: "100",
		Attestation:      base64.StdEncoding.EncodeToString(client.Attestation(t, challenge.Value, "production")),
		ActivationSecret: "synthetic-secret"}
	for _, secret := range []string{"", "wrong-secret"} {
		invalid := request
		invalid.ActivationSecret = secret
		if _, err := service.Register(ctx, invalid); !errors.Is(err, ErrEnrollmentDenied) {
			t.Fatalf("invalid secret accepted: %v", err)
		}
	}
	unlisted := request
	unlisted.Build = "101"
	if _, err := service.Register(ctx, unlisted); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("unlisted build accepted: %v", err)
	}
	if _, err := keys.Get(ctx, client.KeyID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("denied registration stored a key: %v", err)
	}
	principal, err := service.Register(ctx, request)
	if err != nil {
		t.Fatalf("valid production proof on restricted service: %v", err)
	}
	stored, err := keys.Get(ctx, client.KeyID)
	if err != nil || stored.Environment != string(EnvironmentProduction) || stored.DeviceID != principal.DeviceID {
		t.Fatalf("wrong persisted proof realm: %+v, %v", stored, err)
	}
	// A cached proof cannot skip access checks when recovering a lost response.
	request.ActivationSecret = ""
	if _, err := service.Register(ctx, request); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("recovery bypassed access policy: %v", err)
	}
	request.ActivationSecret = "synthetic-secret"
	if recovered, err := service.Register(ctx, request); err != nil || recovered != principal {
		t.Fatalf("authorized recovery: %+v %v", recovered, err)
	}
}

func TestRestrictedEnrollmentCannotOverrideProofRealmOrIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, bundle, realm, secret string }{
		{"development proof", "bundle.dev", "development", "synthetic-secret"},
		{"formal identity", "bundle", "production", "synthetic-secret"},
		{"missing configured secret", "bundle.dev", "production", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			ctx := context.Background()
			client := appattest.New(t, "TEAM."+tc.bundle, now)
			keys := NewMemoryKeyStore()
			service := NewEnrollmentService(EnrollmentConfig{
				AppID: "health", Environment: EnvironmentProduction,
				RequireActivationSecret: true, DevelopmentSecret: tc.secret,
			}, NewMemoryNonceStore(), keys, NewAppleAttestationVerifier("TEAM", "bundle.dev", EnvironmentProduction, client.Roots), func() time.Time { return now })
			challenge, err := service.IssueChallenge(ctx, client.KeyID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Register(ctx, RegistrationRequest{KeyID: client.KeyID, Challenge: challenge.Value, ActivationSecret: tc.secret,
				Attestation: base64.StdEncoding.EncodeToString(client.Attestation(t, challenge.Value, tc.realm))})
			if !errors.Is(err, ErrEnrollmentDenied) {
				t.Fatalf("invalid enrollment accepted: %v", err)
			}
			if _, err := keys.Get(ctx, client.KeyID); !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("invalid enrollment persisted: %v", err)
			}
		})
	}
}
