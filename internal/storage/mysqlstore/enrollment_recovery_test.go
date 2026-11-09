package mysqlstore_test

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/storage/redisstore"
	"github.com/tellyouwhat/backend/internal/testutil"
	"github.com/tellyouwhat/backend/internal/testutil/appattest"
)

func TestPersistedEnrollmentRecoversAfterRedisChallengeExpires(t *testing.T) {
	redisURL := os.Getenv("REDIS_TEST_URL")
	if redisURL == "" {
		t.Skip("REDIS_TEST_URL is required")
	}
	db := testutil.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	redis, err := redisstore.Open(ctx, redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { redis.Close() })
	now := time.Now().UTC()
	client := appattest.New(t, "TEAM.bundle", now)
	keys := mysqlstore.NewKeyRepository(db, "health")
	service := attestation.NewEnrollmentService(attestation.EnrollmentConfig{AppID: "health", Environment: attestation.EnvironmentProduction}, redisstore.NewNonceStore(redis, "health"), keys,
		attestation.NewAppleAttestationVerifier("TEAM", "bundle", attestation.EnvironmentProduction, client.Roots), func() time.Time { return now })
	challenge, err := service.IssueChallenge(ctx, client.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	nonceKey := "platform:health:attest:nonce:" + challenge.Value
	t.Cleanup(func() { redis.Del(context.Background(), nonceKey) })
	request := attestation.RegistrationRequest{KeyID: client.KeyID, Challenge: challenge.Value, Attestation: base64.StdEncoding.EncodeToString(client.Attestation(t, challenge.Value, "production"))}
	original, err := service.Register(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.AdvanceCounter(ctx, client.KeyID, 0, 41); err != nil {
		t.Fatal(err)
	}
	if err := keys.BindTransaction(ctx, client.KeyID, "verified-purchase"); err != nil {
		t.Fatal(err)
	}
	// Redis deletes expired values. Exercise the same missing-nonce path without
	// slowing the suite down by waiting for the five-minute enrollment TTL.
	if err := redis.Del(ctx, nonceKey).Err(); err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Register(ctx, request)
	if err != nil || recovered.DeviceID != original.DeviceID || recovered.TransactionID != "verified-purchase" {
		t.Fatalf("persisted identity recovery failed: %+v %v", recovered, err)
	}
	stored, err := keys.Get(ctx, client.KeyID)
	if err != nil || stored.Counter != 41 {
		t.Fatal("recovery changed the persisted counter")
	}
	request.Challenge = "unrelated-challenge"
	if _, err := service.Register(ctx, request); err == nil {
		t.Fatal("unbound proof recovered a persisted identity")
	}
}
