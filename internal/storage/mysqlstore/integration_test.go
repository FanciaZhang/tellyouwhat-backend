package mysqlstore_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/jobs"
	"github.com/tellyouwhat/backend/internal/media"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/usage"
	"github.com/tellyouwhat/backend/migrations"
)

func TestMySQLPersistencePaths(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("MYSQL_TEST_DSN"))
	if dsn == "" {
		t.Skip("set MYSQL_TEST_DSN to run MySQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := mysqlstore.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var databaseName string
	if err := database.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&databaseName); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(databaseName, "_test") {
		t.Fatalf("refusing to reset non-test database %q", databaseName)
	}
	if err := migrations.Run(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, database); err != nil {
		t.Fatalf("migrations must be idempotent: %v", err)
	}
	var migrationCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 2 {
		t.Fatalf("expected 2 recorded migrations, got %d", migrationCount)
	}
	resetMySQLTables(t, ctx, database)

	now := time.Now().UTC().Truncate(time.Microsecond)
	keyRepository := mysqlstore.NewKeyRepository(database)
	key := attestation.RegisteredKey{
		KeyID: "integration-key", DeviceID: "00000000-0000-4000-8000-000000000001",
		PublicKey: []byte("public-key"), Environment: "production", Receipt: []byte("receipt"),
	}
	if err := keyRepository.Register(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := keyRepository.Register(ctx, key); !errors.Is(err, attestation.ErrKeyAlreadyRegistered) {
		t.Fatalf("duplicate key registration: %v", err)
	}
	if err := keyRepository.AdvanceCounter(ctx, key.KeyID, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := keyRepository.AdvanceCounter(ctx, key.KeyID, 0, 2); !errors.Is(err, attestation.ErrReplay) {
		t.Fatalf("counter replay: %v", err)
	}
	if err := keyRepository.BindTransaction(ctx, key.KeyID, "transaction-1"); err != nil {
		t.Fatal(err)
	}
	if err := keyRepository.BindTransaction(ctx, key.KeyID, "transaction-1"); err != nil {
		t.Fatalf("idempotent transaction binding: %v", err)
	}
	if err := keyRepository.BindTransaction(ctx, key.KeyID, "transaction-2"); !errors.Is(err, attestation.ErrTransactionBindingConflict) {
		t.Fatalf("conflicting transaction binding: %v", err)
	}

	entitlementRepository := mysqlstore.NewEntitlementRepository(database)
	initialExpiry := now.Add(24 * time.Hour)
	if err := entitlementRepository.Upsert(ctx, entitlement.Record{
		KeyID: key.KeyID, TransactionID: "transaction-1", Environment: "production", ExpiresAt: initialExpiry,
	}); err != nil {
		t.Fatal(err)
	}
	notifiedExpiry := initialExpiry.Add(30 * 24 * time.Hour)
	applied, err := entitlementRepository.ApplyNotification(ctx, entitlement.NotificationState{NotificationUUID: "notification-1", OriginalTransactionID: "transaction-1", Environment: "production", ExpiresAt: notifiedExpiry})
	if err != nil || !applied {
		t.Fatalf("apply entitlement notification: applied=%v err=%v", applied, err)
	}
	applied, err = entitlementRepository.ApplyNotification(ctx, entitlement.NotificationState{NotificationUUID: "notification-1", OriginalTransactionID: "transaction-1", Environment: "production", ExpiresAt: notifiedExpiry})
	if err != nil || applied {
		t.Fatalf("notification replay: applied=%v err=%v", applied, err)
	}
	storedEntitlement, ok, err := entitlementRepository.Get(ctx, key.KeyID)
	if err != nil || !ok || !storedEntitlement.ExpiresAt.Equal(notifiedExpiry) {
		t.Fatalf("stored entitlement: %#v ok=%v err=%v", storedEntitlement, ok, err)
	}

	mediaRepository := mysqlstore.NewMediaRepository(database)
	mediaRecord := media.Record{
		ObjectID: "temporary/integration.jpg", OwnerKeyID: key.KeyID,
		OwnerDeviceID: key.DeviceID, RequestID: "00000000-0000-4000-8000-000000000002",
		Operation: contracts.OperationMealPhotoCapture, MediaID: "photo-1", Kind: "image",
		MIMEType: "image/jpeg", SHA256: strings.Repeat("a", 64), SizeBytes: 128, ExpiresAt: now.Add(time.Hour),
	}
	if err := mediaRepository.Register(ctx, mediaRecord); err != nil {
		t.Fatal(err)
	}
	if err := mediaRepository.Register(ctx, mediaRecord); err != nil {
		t.Fatalf("idempotent media registration: %v", err)
	}
	conflictingMedia := mediaRecord
	conflictingMedia.MediaID = "photo-2"
	if err := mediaRepository.Register(ctx, conflictingMedia); !errors.Is(err, media.ErrAuthorizationConflict) {
		t.Fatalf("conflicting media registration: %v", err)
	}
	attempt := media.AttemptRecord{
		RequestID: mediaRecord.RequestID, OwnerKeyID: key.KeyID, BodyDigest: strings.Repeat("b", 64),
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}
	_, replayed, err := mediaRepository.CommitAttempt(ctx, []media.Record{mediaRecord}, attempt, now)
	if err != nil || replayed {
		t.Fatalf("commit media attempt: replayed=%v err=%v", replayed, err)
	}
	_, replayed, err = mediaRepository.CommitAttempt(ctx, []media.Record{mediaRecord}, attempt, now)
	if err != nil || !replayed {
		t.Fatalf("media attempt replay: replayed=%v err=%v", replayed, err)
	}

	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	jobRepository := mysqlstore.NewJobRepository(database, cipher)
	requestID := "00000000-0000-4000-8000-000000000003"
	jobID := "00000000-0000-4000-8000-000000000004"
	request := contracts.Request{
		RequestID: requestID, Operation: contracts.OperationMealTextCapture,
		ContractVersion: contracts.ContractVersionV1, PromptVersion: "meal-text-v4",
		Prompt: "integration", ResponseSchema: json.RawMessage(`{"type":"object"}`),
		SemanticSignature: "integration-v1",
	}
	job := jobs.Job{
		ID: jobID, RequestID: requestID, BodyDigest: strings.Repeat("c", 64),
		OwnerKeyID: key.KeyID, OwnerDeviceID: key.DeviceID, OwnerTransactionID: "transaction-1",
		Request: request, Status: jobs.StatusQueued, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if _, err := jobRepository.CreateOrGet(ctx, job); err != nil {
		t.Fatal(err)
	}
	if existing, err := jobRepository.CreateOrGet(ctx, job); err != nil || existing.ID != job.ID {
		t.Fatalf("idempotent job creation: %#v err=%v", existing, err)
	}
	dispatches, err := jobRepository.ClaimDispatches(ctx, now, 10)
	if err != nil || len(dispatches) != 1 || dispatches[0].JobID != job.ID {
		t.Fatalf("claim durable dispatch: %#v err=%v", dispatches, err)
	}
	if err := jobRepository.CompleteDispatch(ctx, job.ID, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobRepository.Claim(ctx, job.ID, now)
	if err != nil || claimed.AttemptCount != 1 || claimed.Status != jobs.StatusRunning {
		t.Fatalf("claim job: %#v err=%v", claimed, err)
	}
	if err := jobRepository.Succeed(ctx, job.ID, claimed.AttemptCount, providerapi.Response{
		Content: `{"ok":true}`, InputTokens: 12, OutputTokens: 8,
	}, usage.Record{
		RequestID: requestID, KeyID: key.KeyID, DeviceID: key.DeviceID,
		TransactionID: "transaction-1", Operation: request.Operation,
		InputTokens: 12, OutputTokens: 8, OccurredAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	storedJob, err := jobRepository.Get(ctx, job.ID)
	if err != nil || storedJob.Status != jobs.StatusSucceeded || storedJob.Result != `{"ok":true}` {
		t.Fatalf("stored successful job: %#v err=%v", storedJob, err)
	}
	var usageCount, outboxCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_ledger WHERE request_id = ?`, requestID).Scan(&usageCount); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM job_dispatch_outbox WHERE job_id = ?`, jobID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if usageCount != 1 || outboxCount != 0 {
		t.Fatalf("atomic job completion: usage=%d outbox=%d", usageCount, outboxCount)
	}
}

func resetMySQLTables(t *testing.T, ctx context.Context, database *sql.DB) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
        SET FOREIGN_KEY_CHECKS = 0;
        TRUNCATE TABLE app_store_offer_redemptions;
        TRUNCATE TABLE admin_operations;
        TRUNCATE TABLE admin_audit_events;
        TRUNCATE TABLE admin_recovery_codes;
        TRUNCATE TABLE admin_webauthn_credentials;
        TRUNCATE TABLE admin_bootstrap_tokens;
        TRUNCATE TABLE admin_users;
        TRUNCATE TABLE app_store_notifications;
        TRUNCATE TABLE job_dispatch_outbox;
        TRUNCATE TABLE usage_ledger;
        TRUNCATE TABLE media_objects;
        TRUNCATE TABLE idempotency_records;
        TRUNCATE TABLE ai_jobs;
        TRUNCATE TABLE managed_entitlements;
        TRUNCATE TABLE app_attest_keys;
        SET FOREIGN_KEY_CHECKS = 1`)
	if err != nil {
		t.Fatal(err)
	}
}
