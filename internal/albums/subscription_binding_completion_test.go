package albums

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

func completionFixture(t *testing.T) (*pushFixture, *MySQLUploads, *AlbumSubscriptionService, *bindingProvider, accountDeletionRequest) {
	f, r, s, p, d := bindingFixture(t, "stable")
	p.extra = []appstore.SubscriptionStatus{extraBindingStatus(t, p, "second-chain", p.target)}
	payload := p.fixture.payload(p.target)
	payload["originalTransactionId"] = "second-chain"
	payload["transactionId"] = "second-chain-transaction"
	if err := s.receiveNotification(context.Background(), p.fixture.notification(t, uuid.NewString(), "DID_RENEW", payload)); err != nil {
		t.Fatal(err)
	}
	// This is an earlier, individually verified chain. The final pass must query
	// it again and not rely solely on that persisted acknowledgement.
	if _, err := f.r.db.Exec(`UPDATE album_deletion_subscription_jobs SET acknowledged_at=?,verified_signed_at=?,verified_transaction_id='second-chain-transaction' WHERE original_transaction_id='second-chain'`, f.now, f.now); err != nil {
		t.Fatal(err)
	}
	return f, r, s, p, d
}

func TestMySQLSubscriptionBindingCompletionRequeriesEveryChain(t *testing.T) {
	f, r, s, p, d := completionFixture(t)
	queried := map[string]int{}
	p.afterRead = func(id string) { queried[id]++ }
	b, err := s.runSubscriptionBindingBatch(context.Background(), r, 10)
	if err != nil || b.Acknowledged != 1 || queried["1000000000000002"] < 2 || queried["second-chain-transaction"] != 1 || p.puts != 0 {
		t.Fatal(b, err, queried, p.puts)
	}
	var n int
	if err = f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND acknowledged_at IS NOT NULL`, d.AccountID).Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
}

func TestMySQLSubscriptionBindingCompletionRejectsChangedOrMissingEvidence(t *testing.T) {
	for _, mode := range []string{"missing-chain", "legacy-chain", "notification-race", "new-chain", "conflicting-discovery", "expired-lease"} {
		t.Run(mode, func(t *testing.T) {
			f, r, s, p, d := completionFixture(t)
			p.afterRead = func(id string) {
				if id != "second-chain-transaction" {
					return
				}
				p.afterRead = nil
				switch mode {
				case "missing-chain":
					p.extra = nil
				case "legacy-chain":
					p.extra = []appstore.SubscriptionStatus{extraBindingStatus(t, p, "second-chain", d.AccountID)}
				case "notification-race":
					if err := s.receiveNotification(context.Background(), p.fixture.notification(t, uuid.NewString(), "DID_RENEW", p.payload)); err != nil {
						t.Fatal(err)
					}
				case "new-chain":
					p.extra = append(p.extra, extraBindingStatus(t, p, "third-chain", p.target))
				case "conflicting-discovery":
					p.extra = append(p.extra, extraBindingStatus(t, p, "third-chain", p.target), extraBindingStatus(t, p, "third-chain", uuid.NewString()))
				case "expired-lease":
					f.now = f.now.Add(3 * time.Minute)
				}
			}
			b, err := s.runSubscriptionBindingBatch(context.Background(), r, 10)
			if b.Acknowledged != 0 || (err != nil && !errors.Is(err, ErrSubscriptionConflict)) || p.puts != 0 {
				t.Fatal(b, err, p.puts)
			}
			var ack sql.NullTime
			if err = f.r.db.QueryRow(`SELECT acknowledged_at FROM album_deletion_subscription_jobs WHERE account_id=? AND original_transaction_id='1000000000000001'`, d.AccountID).Scan(&ack); err != nil || ack.Valid {
				t.Fatal("stale completion committed", err)
			}
			if revision, checked, _ := readCompletionReceipt(t, f, d.AccountID); revision.Valid || checked.Valid {
				t.Fatal("incomplete verification left a full-set receipt")
			}
			if mode == "new-chain" || mode == "conflicting-discovery" {
				want := 1
				if mode == "conflicting-discovery" {
					want = 0
				}
				var n int
				if err = f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND original_transaction_id='third-chain' AND acknowledged_at IS NULL`, d.AccountID).Scan(&n); err != nil || n != want {
					t.Fatal("unexpected discovered chain count", n, want, err)
				}
			}
		})
	}
}

func TestMySQLSubscriptionBindingCompletionRejectsConcurrentPartialPasses(t *testing.T) {
	f, r, s, p, d := completionFixture(t)
	if _, err := f.r.db.Exec(`UPDATE album_deletion_subscription_jobs SET acknowledged_at=NULL,verified_signed_at=NULL,verified_transaction_id=NULL WHERE account_id=?`, d.AccountID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var jobs []subscriptionBindingJob
	for _, original := range []string{"1000000000000001", "second-chain"} {
		j, err := r.claimSubscriptionBinding(ctx, subscriptionBindingJob{Account: d.AccountID, Request: d.RequestID, Environment: "Sandbox", Original: original, Target: p.target}, f.now)
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
		proof, err := s.verifyBindingCompletion(ctx, r, j)
		if err != nil || proof != nil {
			t.Fatal("partial pass became full proof", proof, err)
		}
	}
	for i, j := range jobs {
		proof, err := s.resolver.resolveCurrent(ctx, j.Account, j.Original, j.Transaction, j.Target)
		if err != nil {
			t.Fatal(err)
		}
		err = r.finishSubscriptionBinding(ctx, j, &proof, nil, f.now)
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if revision, checked, _ := readCompletionReceipt(t, f, d.AccountID); revision.Valid || checked.Valid {
				t.Fatal("partial pass persisted a complete receipt")
			}
		}
		if i == 1 && !errors.Is(err, ErrSubscriptionConflict) {
			t.Fatal("last partial pass acknowledged without complete evidence", err)
		}
	}
	proof, err := s.verifyBindingCompletion(ctx, r, jobs[1])
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.resolver.resolveCurrent(ctx, d.AccountID, jobs[1].Original, jobs[1].Transaction, p.target)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.finishSubscriptionBinding(ctx, jobs[1], &current, proof, f.now); err != nil {
		t.Fatal("fresh final pass rejected", err)
	}
}

func TestBindingCompletionProofIsBoundedAndExact(t *testing.T) {
	now := time.Now().UTC()
	a := bindingCompletionProof{Revision: 2, CheckedAt: now, Anchors: []bindingCompletionAnchor{{"original", "transaction", now}}}
	if !matchingBindingCompletion(a, &a, now) {
		t.Fatal("fresh proof rejected")
	}
	for _, at := range []time.Time{now.Add(-time.Nanosecond), now.Add(time.Minute)} {
		if matchingBindingCompletion(a, &a, at) {
			t.Fatal("out-of-window proof accepted")
		}
	}
	b := a
	b.Revision++
	if matchingBindingCompletion(b, &a, now) || matchingBindingCompletion(a, nil, now) {
		t.Fatal("mismatched proof accepted")
	}
	b = a
	b.Anchors = []bindingCompletionAnchor{{"other", "transaction", now}}
	if matchingBindingCompletion(b, &a, now) {
		t.Fatal("wrong purchase set accepted")
	}
}
