package albums

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func readCompletionReceipt(t *testing.T, f *pushFixture, owner string) (sql.NullInt64, sql.NullTime, int64) {
	t.Helper()
	var revision sql.NullInt64
	var checked sql.NullTime
	var current int64
	if err := f.r.db.QueryRow(`SELECT s.subscription_checked_revision,s.subscription_checked_at,s.subscription_revision
 FROM album_account_deletions d JOIN album_storage_accounts s ON s.owner_id=d.account_id WHERE d.account_id=?`, owner).Scan(&revision, &checked, &current); err != nil {
		t.Fatal(err)
	}
	return revision, checked, current
}

func TestMySQLSubscriptionCompletionReceiptTracksRevisionAndNotification(t *testing.T) {
	f, r, s, p, d := completionFixture(t)
	ctx := context.Background()
	if revision, at, _ := readCompletionReceipt(t, f, d.AccountID); revision.Valid || at.Valid {
		t.Fatal("individual receipt was mistaken for complete verification")
	}
	if b, err := s.runSubscriptionBindingBatch(ctx, r, 10); err != nil || b.Acknowledged != 1 {
		t.Fatal(b, err)
	}
	revision, checked, current := readCompletionReceipt(t, f, d.AccountID)
	if !revision.Valid || revision.Int64 != current || !checked.Valid || !checked.Time.Equal(f.now) {
		t.Fatal("missing complete revision receipt")
	}
	// A later verified notification and invalidation commit together.
	f.now = f.now.Add(time.Second)
	p.payload["signedDate"] = f.now.UnixMilli()
	if err := s.receiveNotification(ctx, p.fixture.notification(t, uuid.NewString(), "DID_RENEW", p.payload)); err != nil {
		t.Fatal(err)
	}
	if next, at, version := readCompletionReceipt(t, f, d.AccountID); next.Valid || at.Valid || version != current+1 {
		t.Fatal("notification left a stale complete receipt")
	}
	if b, err := s.runSubscriptionBindingBatch(ctx, r, 10); err != nil || b.Acknowledged != 1 {
		t.Fatal(b, err)
	}
	if next, at, version := readCompletionReceipt(t, f, d.AccountID); !next.Valid || next.Int64 != version || !at.Time.Equal(f.now) {
		t.Fatal("fresh full recheck not persisted")
	}
}

func TestMySQLSubscriptionCompletionReceiptAndInvalidationRollBackTogether(t *testing.T) {
	for _, mode := range []string{"receipt-write", "invalidation"} {
		t.Run(mode, func(t *testing.T) {
			f, r, s, p, d := completionFixture(t)
			ctx := context.Background()
			if mode == "receipt-write" {
				if _, err := f.r.db.Exec(`ALTER TABLE album_storage_accounts ADD CONSTRAINT fail_completion_write CHECK(subscription_checked_revision IS NULL)`); err != nil {
					t.Fatal(err)
				}
				if _, err := s.runSubscriptionBindingBatch(ctx, r, 10); err == nil {
					t.Fatal("failed complete receipt accepted")
				}
				var ack sql.NullTime
				if err := f.r.db.QueryRow(`SELECT acknowledged_at FROM album_deletion_subscription_jobs WHERE account_id=? AND original_transaction_id='1000000000000001'`, d.AccountID).Scan(&ack); err != nil || ack.Valid {
					t.Fatal("last individual receipt escaped rollback", err)
				}
				if revision, at, _ := readCompletionReceipt(t, f, d.AccountID); revision.Valid || at.Valid {
					t.Fatal("partial complete receipt")
				}
				return
			}
			if b, err := s.runSubscriptionBindingBatch(ctx, r, 10); err != nil || b.Acknowledged != 1 {
				t.Fatal(b, err)
			}
			before, at, version := readCompletionReceipt(t, f, d.AccountID)
			if _, err := f.r.db.Exec(`ALTER TABLE album_storage_accounts ADD CONSTRAINT fail_completion_clear CHECK(subscription_checked_revision IS NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(time.Second)
			p.payload["signedDate"] = f.now.UnixMilli()
			id := uuid.NewString()
			if err := s.receiveNotification(ctx, p.fixture.notification(t, id, "DID_RENEW", p.payload)); err == nil {
				t.Fatal("notification committed without invalidation")
			}
			after, checked, current := readCompletionReceipt(t, f, d.AccountID)
			if before != after || !at.Time.Equal(checked.Time) || current != version {
				t.Fatal("notification partially changed completion state")
			}
			var count int
			if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_subscription_notifications WHERE notification_id=?`, id).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed notification was consumed", err)
			}
			if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND acknowledged_at IS NOT NULL`, d.AccountID).Scan(&count); err != nil || count != 2 {
				t.Fatal("invalidation failed to roll back job updates", err)
			}
		})
	}
}

func TestMySQLSubscriptionCompletionReceiptMigrationRechecksOldAcknowledgements(t *testing.T) {
	f, r, s, _, d := completionFixture(t)
	ctx := context.Background()
	if b, err := s.runSubscriptionBindingBatch(ctx, r, 10); err != nil || b.Acknowledged != 1 {
		t.Fatal(b, err)
	}
	if _, err := f.r.db.Exec(`ALTER TABLE album_storage_accounts DROP CHECK album_subscription_completion_receipt, DROP COLUMN subscription_checked_revision, DROP COLUMN subscription_checked_at`); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckSubscriptionSchema(ctx); err == nil {
		t.Fatal("old schema accepted")
	}
	if _, err := f.r.db.Exec(`DELETE FROM schema_migrations WHERE name='0062_album_subscription_completion_receipt.sql'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Run(ctx, f.r.db); err != nil {
			t.Fatal(err)
		}
	}
	if revision, at, _ := readCompletionReceipt(t, f, d.AccountID); revision.Valid || at.Valid {
		t.Fatal("migration invented complete evidence")
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND acknowledged_at IS NULL AND verified_signed_at IS NULL AND verified_transaction_id IS NULL`, d.AccountID).Scan(&count); err != nil || count != 2 {
		t.Fatal("old acknowledgements not requeued", err)
	}
	if err := r.CheckSubscriptionSchema(ctx); err != nil {
		t.Fatal(err)
	}
	// SQL migration uses the database clock; move the controlled worker clock
	// past that requeue time before expecting both jobs to be due.
	f.now = f.now.Add(time.Minute)
	if b, err := s.runSubscriptionBindingBatch(ctx, r, 10); err != nil || b.Acknowledged != 2 {
		t.Fatal(b, err)
	}
	if revision, at, current := readCompletionReceipt(t, f, d.AccountID); !revision.Valid || revision.Int64 != current || !at.Valid {
		t.Fatal("migrated jobs did not reach full verification")
	}
}

func TestGatewaySubscriptionCompletionReceiptSchema(t *testing.T) {
	f := newAppleGatewayFixture(t)
	if _, err := f.db.Exec(`ALTER TABLE album_storage_accounts DROP CHECK album_subscription_completion_receipt, DROP COLUMN subscription_checked_revision`); err != nil {
		t.Fatal(err)
	}
	g, err := NewGateway(context.Background(), f.config, nil)
	if g != nil {
		g.Close()
	}
	if err == nil {
		t.Fatal("gateway started without completion schema")
	}
}
