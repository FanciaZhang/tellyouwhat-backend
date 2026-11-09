package mysqlstore_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func testPrivacyDeletionReceipts(t *testing.T, ctx context.Context, database *sql.DB, now time.Time) {
	t.Helper()
	const appID = "privacy-receipt-test"
	const otherAppID = "privacy-receipt-other-test"
	for _, app := range []string{appID, otherAppID} {
		if _, err := database.ExecContext(ctx, `INSERT INTO apps (app_id, display_name, bundle_id, managed_product_id) VALUES (?, ?, ?, ?)`, app, app, app, app); err != nil {
			t.Fatal(err)
		}
	}
	keys := mysqlstore.NewKeyRepository(database, appID)
	owner := attestation.RegisteredKey{AppID: appID, KeyID: "receipt-owner", DeviceID: "00000000-0000-4000-8000-000000000071", TransactionID: "receipt-transaction", PublicKey: []byte("synthetic-public-key"), Environment: "production", Receipt: []byte("synthetic-attestation")}
	sibling := owner
	sibling.KeyID, sibling.DeviceID = "receipt-sibling", "00000000-0000-4000-8000-000000000072"
	for _, key := range []attestation.RegisteredKey{owner, sibling} {
		if err := keys.Register(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	otherKeys := mysqlstore.NewKeyRepository(database, otherAppID)
	if err := otherKeys.Register(ctx, owner); err != nil {
		t.Fatal(err)
	}
	repository := mysqlstore.NewPrivacyRepository(database, appID)
	if err := repository.RecordConsents(ctx, []privacy.Record{{KeyID: owner.KeyID, DeviceID: owner.DeviceID, Scope: privacy.PrivacyTermsScope, DocumentVersion: privacy.GeneralDocumentVersion, Granted: true, RecordedAt: now}}); err != nil {
		t.Fatal(err)
	}
	principal := attestation.Principal{AppID: appID, KeyID: owner.KeyID, DeviceID: owner.DeviceID, TransactionID: owner.TransactionID}
	receipt, err := privacy.NewDeletionReceipt(appID, attestation.RequestProof{Method: "DELETE", Path: "/v1/privacy/data", RequestID: "receipt-request", KeyID: owner.KeyID, Assertion: "synthetic-proof", Nonce: "synthetic-nonce", Timestamp: now.Format(time.RFC3339), BodySHA256: "synthetic-empty-body-hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `ALTER TABLE privacy_deletion_receipts ADD CONSTRAINT fail_privacy_receipt CHECK (app_id <> 'privacy-receipt-test')`); err != nil {
		t.Fatal(err)
	}
	defer database.ExecContext(context.Background(), `ALTER TABLE privacy_deletion_receipts DROP CHECK fail_privacy_receipt`)
	if err := repository.DeletePrincipalWithReceipt(ctx, principal, receipt); err == nil {
		t.Fatal("receipt write failure must fail deletion")
	}
	if completed, err := repository.DeletionCompleted(ctx, receipt); err != nil || completed {
		t.Fatalf("rolled-back transaction reported completion: %v %v", completed, err)
	}
	for _, key := range []attestation.RegisteredKey{owner, sibling} {
		if _, err := keys.Get(ctx, key.KeyID); err != nil {
			t.Fatalf("receipt failure did not restore the original identity: %v", err)
		}
	}
	var consentCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM privacy_consents WHERE app_id = ?`, appID).Scan(&consentCount); err != nil || consentCount != 1 {
		t.Fatalf("cascaded consent deletion did not roll back: count=%d err=%v", consentCount, err)
	}
	if _, err := database.ExecContext(ctx, `ALTER TABLE privacy_deletion_receipts DROP CHECK fail_privacy_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeletePrincipalWithReceipt(ctx, principal, receipt); err != nil {
		t.Fatal(err)
	}
	restarted := mysqlstore.NewPrivacyRepository(database, appID)
	if completed, err := restarted.DeletionCompleted(ctx, receipt); err != nil || !completed {
		t.Fatalf("completion did not survive repository restart: %v %v", completed, err)
	}
	for _, key := range []attestation.RegisteredKey{owner, sibling} {
		if _, err := keys.Get(ctx, key.KeyID); !errors.Is(err, attestation.ErrKeyNotFound) {
			t.Fatalf("identity survived successful deletion: %v", err)
		}
	}
	if _, err := otherKeys.Get(ctx, owner.KeyID); err != nil {
		t.Fatalf("deletion crossed applications: %v", err)
	}
	if completed, err := mysqlstore.NewPrivacyRepository(database, otherAppID).DeletionCompleted(ctx, receipt); err != nil || completed {
		t.Fatalf("completion crossed applications: %v %v", completed, err)
	}
	if err := restarted.DeletePrincipalWithReceipt(ctx, principal, receipt); err != nil {
		t.Fatalf("duplicate completion was not idempotent: %v", err)
	}
	var receiptCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM privacy_deletion_receipts WHERE app_id = ?`, appID).Scan(&receiptCount); err != nil || receiptCount != 1 {
		t.Fatalf("duplicate completion created extra receipts: count=%d err=%v", receiptCount, err)
	}
}
