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

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tellyouwhat/backend/internal/adminauth"
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
	if migrationCount < 1 {
		t.Fatalf("expected at least 1 recorded migration, got %d", migrationCount)
	}
	resetMySQLTables(t, ctx, database)
	const appID = "health"
	if _, err := database.ExecContext(ctx, `
		INSERT INTO apps (app_id, display_name, bundle_id, managed_product_id)
		VALUES (?, ?, ?, ?)`, appID, "告你健康", "cn.tellyouwhat.healthapp", "health.premium.subscription.monthly"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	testAdminPersistence(t, ctx, database, now, appID)
	keyRepository := mysqlstore.NewKeyRepository(database, appID)
	key := attestation.RegisteredKey{
		AppID: appID, KeyID: "integration-key", DeviceID: "00000000-0000-4000-8000-000000000001",
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

	entitlementRepository := mysqlstore.NewEntitlementRepository(database, appID)
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

	mediaRepository := mysqlstore.NewMediaRepository(database, appID)
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
	jobRepository := mysqlstore.NewJobRepository(database, cipher, appID)
	requestID := "00000000-0000-4000-8000-000000000003"
	jobID := "00000000-0000-4000-8000-000000000004"
	request := contracts.Request{
		RequestID: requestID, Operation: contracts.OperationMealTextCapture,
		ContractVersion: contracts.ContractVersionV1, PromptVersion: "meal-text-v4",
		Prompt: "integration", ResponseSchema: json.RawMessage(`{"type":"object"}`),
		SemanticSignature: "integration-v1",
	}
	job := jobs.Job{
		AppID: appID, ID: jobID, RequestID: requestID, BodyDigest: strings.Repeat("c", 64),
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
	t.Run("reject_legacy_migration_collision", func(t *testing.T) {
		testLegacyMigrationCollision(t, ctx, database)
	})
}

func testLegacyMigrationCollision(t *testing.T, ctx context.Context, database *sql.DB) {
	t.Helper()
	if _, err := database.ExecContext(ctx, `
        RENAME TABLE apps TO migration_guard_saved_apps,
                     app_attest_keys TO migration_guard_saved_keys`); err != nil {
		t.Fatal(err)
	}
	legacyCreated := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if legacyCreated {
			if _, err := database.ExecContext(cleanupCtx, `DROP TABLE app_attest_keys`); err != nil {
				t.Errorf("remove legacy test fixture: %v", err)
				return
			}
		}
		if _, err := database.ExecContext(cleanupCtx, `
            RENAME TABLE migration_guard_saved_apps TO apps,
                         migration_guard_saved_keys TO app_attest_keys`); err != nil {
			t.Errorf("restore shared test schema: %v", err)
		}
	}()
	if _, err := database.ExecContext(ctx, `
        CREATE TABLE app_attest_keys (key_id VARCHAR(64) PRIMARY KEY, public_key_der LONGBLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	legacyCreated = true
	if _, err := database.ExecContext(ctx, `INSERT INTO app_attest_keys VALUES ('legacy-test-key', 'legacy-test-value')`); err != nil {
		t.Fatal(err)
	}
	var beforeMigrations int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&beforeMigrations); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, database); err == nil || !strings.Contains(err.Error(), "legacy single-app schema") {
		t.Fatalf("expected legacy schema rejection despite an applied initial migration, got %v", err)
	}
	var storedKey string
	if err := database.QueryRowContext(ctx, `SELECT public_key_der FROM app_attest_keys WHERE key_id = 'legacy-test-key'`).Scan(&storedKey); err != nil || storedKey != "legacy-test-value" {
		t.Fatalf("legacy data must remain unchanged: value=%q err=%v", storedKey, err)
	}
	var appsTables, afterMigrations int
	if err := database.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'apps'`).Scan(&appsTables); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&afterMigrations); err != nil {
		t.Fatal(err)
	}
	if appsTables != 0 || afterMigrations != beforeMigrations {
		t.Fatalf("legacy rejection mutated the schema: apps=%d migrations=%d (before %d)", appsTables, afterMigrations, beforeMigrations)
	}
}

func testAdminPersistence(t *testing.T, ctx context.Context, database *sql.DB, now time.Time, appID string) {
	t.Helper()
	repository := adminauth.NewMySQLRepository(database)
	bootstrapToken := adminauth.TokenHash("bootstrap-integration-token")
	if err := repository.CreateBootstrapToken(ctx, bootstrapToken, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	admin := adminauth.User{
		ID: "10000000-0000-4000-8000-000000000001", Handle: []byte("administrator-handle"),
		DisplayName: "管理员", Role: adminauth.RoleAdmin, Status: adminauth.UserStatusActive, SessionVersion: 1,
	}
	if err := repository.CompleteBootstrap(ctx, bootstrapToken, admin,
		webauthn.Credential{ID: []byte("administrator-passkey")}, now); err != nil {
		t.Fatal(err)
	}
	if initialized, err := repository.Initialized(ctx); err != nil || !initialized {
		t.Fatalf("admin bootstrap: initialized=%v err=%v", initialized, err)
	}

	inviteToken := adminauth.TokenHash("operator-integration-invitation")
	invitation := adminauth.Invitation{
		ID: "10000000-0000-4000-8000-000000000002", Kind: adminauth.InvitationKindCreate,
		InvitedByID: admin.ID, DisplayName: "运营", Role: adminauth.RoleOperator, AppIDs: []string{appID},
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := repository.CreateInvitation(ctx, inviteToken, invitation); err != nil {
		t.Fatal(err)
	}
	storedInvitation, found, err := repository.InvitationByToken(ctx, inviteToken, now)
	if err != nil || !found || len(storedInvitation.AppIDs) != 1 || storedInvitation.AppIDs[0] != appID {
		t.Fatalf("stored invitation: %#v found=%v err=%v", storedInvitation, found, err)
	}
	operatorCandidate := adminauth.User{
		ID: "10000000-0000-4000-8000-000000000003", Handle: []byte("operator-handle"),
	}
	operator, err := repository.CompleteInvitation(ctx, inviteToken, storedInvitation, operatorCandidate,
		webauthn.Credential{ID: []byte("operator-passkey-one")}, now)
	if err != nil || operator.Role != adminauth.RoleOperator || !operator.CanAccessApp(appID) {
		t.Fatalf("operator enrollment: %#v err=%v", operator, err)
	}
	operator, err = repository.UpdateUser(ctx, operator.ID, adminauth.UserUpdate{
		DisplayName: "运营同学", Role: operator.Role, Status: operator.Status, AppIDs: operator.AppIDs,
	}, now)
	if err != nil || operator.SessionVersion != 1 {
		t.Fatalf("display-name-only update invalidated sessions: %#v err=%v", operator, err)
	}
	operator, err = repository.UpdateUser(ctx, operator.ID, adminauth.UserUpdate{
		DisplayName: operator.DisplayName, Role: operator.Role,
		Status: adminauth.UserStatusDisabled, AppIDs: operator.AppIDs,
	}, now)
	if err != nil || operator.SessionVersion != 2 {
		t.Fatalf("disable did not invalidate sessions: %#v err=%v", operator, err)
	}
	operator, err = repository.UpdateUser(ctx, operator.ID, adminauth.UserUpdate{
		DisplayName: operator.DisplayName, Role: operator.Role,
		Status: adminauth.UserStatusActive, AppIDs: operator.AppIDs,
	}, now)
	if err != nil || operator.SessionVersion != 3 {
		t.Fatalf("re-enable did not invalidate sessions: %#v err=%v", operator, err)
	}
	if _, err := repository.UpdateUser(ctx, admin.ID, adminauth.UserUpdate{
		DisplayName: admin.DisplayName, Role: adminauth.RoleOperator,
		Status: adminauth.UserStatusActive, AppIDs: []string{appID},
	}, now); !errors.Is(err, adminauth.ErrLastAdmin) {
		t.Fatalf("last administrator was demoted: %v", err)
	}

	recoveryToken := adminauth.TokenHash("operator-integration-recovery")
	recovery := adminauth.Invitation{
		ID: "10000000-0000-4000-8000-000000000004", Kind: adminauth.InvitationKindRecovery,
		TargetUserID: operator.ID, InvitedByID: admin.ID, DisplayName: operator.DisplayName,
		Role: operator.Role, AppIDs: operator.AppIDs, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := repository.CreateInvitation(ctx, recoveryToken, recovery); err != nil {
		t.Fatal(err)
	}
	lockedOperator, found, err := repository.UserByID(ctx, operator.ID)
	if err != nil || !found || len(lockedOperator.Credentials) != 0 || lockedOperator.SessionVersion != 4 {
		t.Fatalf("recovery did not revoke old access: %#v found=%v err=%v", lockedOperator, found, err)
	}
	storedRecovery, found, err := repository.InvitationByToken(ctx, recoveryToken, now)
	if err != nil || !found {
		t.Fatalf("stored recovery: found=%v err=%v", found, err)
	}
	recovered, err := repository.CompleteInvitation(ctx, recoveryToken, storedRecovery, lockedOperator,
		webauthn.Credential{ID: []byte("operator-passkey-two")}, now)
	if err != nil || recovered.SessionVersion != 5 || len(recovered.Credentials) != 1 {
		t.Fatalf("complete recovery: %#v err=%v", recovered, err)
	}
	if err := repository.AddCredential(ctx, operator.ID, "过期会话",
		webauthn.Credential{ID: []byte("stale-session-passkey")}, recovered.SessionVersion-1); !errors.Is(err, adminauth.ErrAuthorizationChanged) {
		t.Fatalf("stale session added a passkey after recovery: %v", err)
	}
	if err := repository.AddCredential(ctx, operator.ID, "备用",
		webauthn.Credential{ID: []byte("operator-passkey-three")}, recovered.SessionVersion); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteCredential(ctx, operator.ID, []byte("operator-passkey-two")); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteCredential(ctx, operator.ID, []byte("operator-passkey-three")); !errors.Is(err, adminauth.ErrLastCredential) {
		t.Fatalf("last passkey was removable: %v", err)
	}
	if err := repository.AppendAudit(ctx, adminauth.AuditEvent{
		UserID: admin.ID, RequestID: "10000000-0000-4000-8000-000000000005",
		Action: "admin.integration", Outcome: "succeeded", Metadata: map[string]any{"safe": true},
	}); err != nil {
		t.Fatal(err)
	}
	events, err := repository.ListAudit(ctx, admin.ID, true, 10)
	if err != nil || len(events) != 1 || events[0].Action != "admin.integration" {
		t.Fatalf("audit persistence: %#v err=%v", events, err)
	}
}

func resetMySQLTables(t *testing.T, ctx context.Context, database *sql.DB) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
        SET FOREIGN_KEY_CHECKS = 0;
        TRUNCATE TABLE app_store_offer_redemptions;
        TRUNCATE TABLE admin_operations;
        TRUNCATE TABLE admin_audit_events;
		TRUNCATE TABLE admin_invitation_apps;
		TRUNCATE TABLE admin_invitations;
		TRUNCATE TABLE admin_user_apps;
        TRUNCATE TABLE admin_webauthn_credentials;
        TRUNCATE TABLE admin_bootstrap_tokens;
        TRUNCATE TABLE admin_users;
		UPDATE admin_control_state SET initialized_at = NULL WHERE singleton_id = 1;
        TRUNCATE TABLE app_store_notifications;
        TRUNCATE TABLE job_dispatch_outbox;
        TRUNCATE TABLE usage_ledger;
        TRUNCATE TABLE media_objects;
        TRUNCATE TABLE idempotency_records;
        TRUNCATE TABLE ai_jobs;
        TRUNCATE TABLE managed_entitlements;
        TRUNCATE TABLE app_attest_keys;
		TRUNCATE TABLE apps;
        SET FOREIGN_KEY_CHECKS = 1`)
	if err != nil {
		t.Fatal(err)
	}
}
