package albums

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func TestMySQLAccountDeletionLedgerIsAtomicAndReplayStable(t *testing.T) {
	f := newPushFixture(t)
	session, _ := f.login(t, "ledger-owner", f.now)
	if _, err := f.r.db.Exec(`ALTER TABLE album_deletion_ledger ADD CONSTRAINT reject_ledger CHECK(account_id='never')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.accounts.requestAccountDeletion(context.Background(), session.Token, uuid.NewString(), func() time.Time { return f.now }); err == nil {
		t.Fatal("accepted without durable ledger")
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_account_deletions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial deletion receipt", count, err)
	}
	if !deletionAuthenticated(f, session.Token) {
		t.Fatal("failed ledger write revoked session")
	}
	if _, err := f.r.db.Exec(`ALTER TABLE album_deletion_ledger DROP CHECK reject_ledger`); err != nil {
		t.Fatal(err)
	}
	d := acceptAppleDeletion(t, f, session.Token)
	if _, err := f.accounts.requestAccountDeletion(context.Background(), session.Token, d.RequestID, func() time.Time { return f.now }); err != nil {
		t.Fatal(err)
	}
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_ledger l JOIN album_account_deletions d ON d.account_id=l.account_id AND d.request_id=l.request_id AND d.authorizing_session_sha256=l.authorizing_session_sha256 AND d.requested_at=l.requested_at`).Scan(&count); err != nil || count != 1 {
		t.Fatal("ledger mismatch or duplicate", count, err)
	}
}

func TestMySQLAccountDeletionLedgerMigrationBackfillsAndHasNoAccountFK(t *testing.T) {
	f := newPushFixture(t)
	session, _ := f.login(t, "ledger-existing", f.now)
	d := acceptAppleDeletion(t, f, session.Token)
	if _, err := f.r.db.Exec(`DROP TABLE album_deletion_ledger`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.db.Exec(`DELETE FROM schema_migrations WHERE name='0060_album_deletion_ledger.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(context.Background(), f.r.db); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(context.Background(), f.r.db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_ledger WHERE account_id=? AND request_id=?`, d.AccountID, d.RequestID).Scan(&count); err != nil || count != 1 {
		t.Fatal("backfill missing", count, err)
	}
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM information_schema.key_column_usage WHERE table_schema=DATABASE() AND table_name='album_deletion_ledger' AND referenced_table_name IS NOT NULL`).Scan(&count); err != nil || count != 0 {
		t.Fatal("ledger depends on live account", count, err)
	}
}

func TestGatewayAccountDeletionLedgerMissingSchemaRejectsStartup(t *testing.T) {
	f := newAppleGatewayFixture(t)
	if _, err := f.db.Exec(`ALTER TABLE album_deletion_ledger RENAME COLUMN requested_at TO hidden_requested_at`); err != nil {
		t.Fatal(err)
	}
	g, err := NewGateway(context.Background(), f.config, nil)
	if g != nil {
		g.Close()
	}
	if err == nil {
		t.Fatal("missing ledger schema accepted")
	}
}
