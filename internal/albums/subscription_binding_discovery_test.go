package albums

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

func extraBindingStatus(t *testing.T, p *bindingProvider, original, token string) appstore.SubscriptionStatus {
	t.Helper()
	payload := p.fixture.payload(token)
	payload["originalTransactionId"] = original
	payload["transactionId"] = original + "-transaction"
	if original == "1000000000000001" {
		payload["transactionId"] = "1000000000000002"
	}
	return appstore.SubscriptionStatus{Status: 1, SignedTransaction: p.fixture.sign(t, payload)}
}

func TestMySQLSubscriptionBindingDiscoversRelatedChainsAndResumes(t *testing.T) {
	f, r, s, p, d := bindingFixture(t, "legacy")
	p.extra = []appstore.SubscriptionStatus{
		extraBindingStatus(t, p, "new-legacy", d.AccountID),
		extraBindingStatus(t, p, "new-stable", p.target),
		extraBindingStatus(t, p, "other-account", uuid.NewString()),
	}
	ctx := context.Background()
	if b, err := s.runSubscriptionBindingBatch(ctx, r, 10); err != nil || b.Acknowledged != 1 {
		t.Fatal(b, err)
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND request_id=? AND target_token=?`, d.AccountID, d.RequestID, p.target).Scan(&count); err != nil || count != 3 {
		t.Fatal("related chains were not persisted", count, err)
	}
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_cloud_subscriptions WHERE original_transaction_id='other-account'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("foreign subscription imported", count, err)
	}
	var before int64
	if err := f.r.db.QueryRow(`SELECT subscription_revision FROM album_storage_accounts WHERE owner_id=?`, d.AccountID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Overlapping responses on restart must neither duplicate discoveries nor
	// invalidate a different chain's acknowledgement.
	for _, original := range []string{"new-legacy", "new-stable"} {
		p.payload = p.fixture.payload(d.AccountID)
		p.payload["originalTransactionId"] = original
		p.payload["transactionId"] = original + "-transaction"
		if original == "new-stable" {
			p.payload["appAccountToken"] = p.target
		}
		p.extra = []appstore.SubscriptionStatus{extraBindingStatus(t, p, "1000000000000001", p.target)}
		if original == "new-stable" {
			p.extra = append(p.extra, extraBindingStatus(t, p, "new-legacy", p.target))
		}
		if b, err := s.runSubscriptionBindingBatch(ctx, r, 1); err != nil || b.Acknowledged != 1 {
			t.Fatal("restart could not reconcile discovered chain", original, b, err)
		}
	}
	if p.puts != 2 {
		t.Fatal("stable or acknowledged binding mutated again", p.puts)
	}
	var after int64
	if err := f.r.db.QueryRow(`SELECT subscription_revision FROM album_storage_accounts WHERE owner_id=?`, d.AccountID).Scan(&after); err != nil || before != after {
		t.Fatal("overlapping response rewrote known chain", before, after, err)
	}
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND acknowledged_at IS NOT NULL`, d.AccountID).Scan(&count); err != nil || count != 3 {
		t.Fatal("not all discoveries acknowledged", count, err)
	}
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_accounts WHERE account_id=? AND state='deleting'`, d.AccountID).Scan(&count); err != nil || count != 1 {
		t.Fatal("discovery reopened account", count, err)
	}
}

func TestMySQLSubscriptionBindingDiscoveryRejectsIncompleteOrConflictingEvidence(t *testing.T) {
	for _, mode := range []string{"forged", "renewal", "status", "mixed-owner", "future", "too-many", "foreign-database-owner", "rollback", "stale-lease"} {
		t.Run(mode, func(t *testing.T) {
			f, r, s, p, d := bindingFixture(t, "legacy")
			p.extra = []appstore.SubscriptionStatus{extraBindingStatus(t, p, "a-discovered", d.AccountID)}
			switch mode {
			case "forged":
				p.extra = append(p.extra, appstore.SubscriptionStatus{Status: 1, SignedTransaction: "invalid"})
			case "renewal":
				p.extra[0].SignedRenewal = "invalid"
			case "status":
				p.extra[0].Status = 4
			case "mixed-owner":
				p.extra = append(p.extra, extraBindingStatus(t, p, "a-discovered", uuid.NewString()))
			case "future":
				payload := p.fixture.payload(d.AccountID)
				payload["originalTransactionId"] = "z-future"
				payload["signedDate"] = f.now.Add(time.Hour).UnixMilli()
				p.extra = append(p.extra, appstore.SubscriptionStatus{Status: 1, SignedTransaction: p.fixture.sign(t, payload)})
			case "too-many":
				for i := 0; i < 128; i++ {
					p.extra = append(p.extra, extraBindingStatus(t, p, fmt.Sprintf("chain-%03d", i), d.AccountID))
				}
			case "foreign-database-owner":
				_, other := f.login(t, "discovery-other", f.now)
				_, err := r.ApplySubscription(context.Background(), other.AccountID, 0, VerifiedAlbumSubscription{"z-occupied", "z-transaction", Cloud50GBAnnualProduct, "Sandbox", f.now.AddDate(1, 0, 0), f.now, true}, f.now)
				if err != nil {
					t.Fatal(err)
				}
				p.extra = append(p.extra, extraBindingStatus(t, p, "z-occupied", d.AccountID))
			case "rollback":
				if _, err := f.r.db.Exec(`ALTER TABLE album_deletion_subscription_jobs ADD CONSTRAINT reject_discovery CHECK(original_transaction_id<>'a-discovered')`); err != nil {
					t.Fatal(err)
				}
			case "stale-lease":
				p.beforePut = func() {
					if _, err := f.r.db.Exec(`UPDATE album_deletion_subscription_jobs SET lease_token=? WHERE account_id=?`, uuid.NewString(), d.AccountID); err != nil {
						t.Fatal(err)
					}
				}
			}
			b, err := s.runSubscriptionBindingBatch(context.Background(), r, 10)
			if err != nil || b.Acknowledged != 0 || p.puts != 0 {
				t.Fatal("partial evidence permitted mutation", b, err, p.puts)
			}
			var count int
			if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_cloud_subscriptions WHERE owner_id=?`, d.AccountID).Scan(&count); err != nil || count != 1 {
				t.Fatal("discovery partially committed", count, err)
			}
			if bindingAcknowledged(t, f.r.db, d.AccountID) {
				t.Fatal("incomplete observation acknowledged")
			}
		})
	}
}
