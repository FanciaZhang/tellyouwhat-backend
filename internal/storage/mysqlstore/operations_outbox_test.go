package mysqlstore_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/jobs"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

func TestMySQLPausedDeliveryPreservesQueueAndOriginalExpiry(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 123456789, time.UTC)
	start := now
	key := attestation.RegisteredKey{AppID: "health", KeyID: "fixture", DeviceID: uuid.NewString(), PublicKey: []byte("fixture"), Environment: "production", Receipt: []byte("fixture")}
	if err := mysqlstore.NewKeyRepository(db, "health").Register(ctx, key); err != nil {
		t.Fatal(err)
	}
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	store := mysqlstore.NewJobRepository(db, cipher, "health")
	service := jobs.NewService(store, func() time.Time { return start })
	request := contracts.Request{RequestID: uuid.NewString(), Operation: contracts.OperationMealDecision, ContractVersion: contracts.ContractVersionV1, PromptVersion: "fixture", Prompt: "choose dinner", ResponseSchema: json.RawMessage(`{"type":"object"}`), SemanticSignature: "sha256:fixture"}
	job, err := service.Enqueue(ctx, attestation.Principal{KeyID: key.KeyID, DeviceID: key.DeviceID}, request, "digest")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		items, err := store.ClaimDispatches(ctx, now, 10)
		if err != nil || len(items) != 1 {
			t.Fatalf("delivery %d: %+v %v", i, items, err)
		}
		if err = store.RetryDispatch(ctx, job.ID, now, jobs.DispatchDeferred); err != nil {
			t.Fatal(err)
		}
		now = now.Add(31 * time.Second)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil || got.Status != jobs.StatusQueued || got.AttemptCount != 0 {
		t.Fatalf("paused: %+v %v", got, err)
	}
	originalExpiry := got.ExpiresAt
	var attempts int
	if err = db.QueryRow(`SELECT attempts FROM job_dispatch_outbox WHERE app_id='health' AND job_id=?`, job.ID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("delivery attempts=%d %v", attempts, err)
	}
	got, err = store.Claim(ctx, job.ID, now)
	if err != nil || got.AttemptCount != 1 {
		t.Fatalf("resume: %+v %v", got, err)
	}
	for i := 0; i < 8; i++ {
		if err = store.DeferAdmission(ctx, job.ID, got.AttemptCount, now); err != nil {
			t.Fatal(err)
		}
		got, err = store.Get(ctx, job.ID)
		if err != nil || got.Status != jobs.StatusQueued || got.AttemptCount != 0 || !got.ExpiresAt.Equal(originalExpiry) {
			t.Fatalf("automatic deferral lost job: %+v %v", got, err)
		}
		got, err = store.Claim(ctx, job.ID, now)
		if err != nil || got.AttemptCount != 1 {
			t.Fatalf("automatic deferral consumed attempt: %+v %v", got, err)
		}
	}
	if _, err = store.ClaimDispatches(ctx, start.Add(25*time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, job.ID)
	if err != nil || got.Status != jobs.StatusFailed || got.FailureCategory != "job_expired" {
		t.Fatalf("expiry: %+v %v", got, err)
	}
}
