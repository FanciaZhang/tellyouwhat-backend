package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/appstore"
)

func TestMySQLExpiredRetentionVerifiesAllChains(t *testing.T) {
	for _, fault := range []string{"none", "renewal", "provider", "unknown-chain", "missing-chain", "expired-proof", "concurrent-renewal"} {
		t.Run(fault, func(t *testing.T) {
			r, owner, now, proofs, _ := reminderVerificationFixture(t)
			now = now.AddDate(0, 0, 30)
			clock := now
			r.billingNow = func() time.Time { return clock }
			before, err := r.CloudAccount(context.Background(), owner, now)
			if err != nil || before.State != "retention_due" {
				t.Fatal(before, err)
			}
			f := newAlbumReceiptFixture(t, now)
			calls := 0
			client := reminderStatusFunc(func(ctx context.Context, id string) ([]appstore.SubscriptionStatus, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 31*time.Second {
					t.Fatal("unbounded Apple request")
				}
				if calls == 2 {
					switch fault {
					case "renewal":
						proofs[0].Active, proofs[0].TransactionID, proofs[0].ExpiresAt = true, "renewed", now.AddDate(1, 0, 0)
					case "provider":
						return nil, errors.New("private provider failure")
					case "unknown-chain":
						unknown := proofs[0]
						unknown.OriginalTransactionID = "unknown-chain"
						return []appstore.SubscriptionStatus{reminderSignedStatus(t, f, owner, proofs[1]), reminderSignedStatus(t, f, owner, unknown)}, nil
					case "missing-chain":
						return nil, nil
					case "expired-proof":
						clock = now.Add(30 * time.Second)
					case "concurrent-renewal":
						renewed := proofs[0]
						renewed.Active, renewed.TransactionID, renewed.ExpiresAt, renewed.SignedAt = true, "concurrent-renewal", now.AddDate(1, 0, 0), now
						applyReminderSubscription(t, r, owner, renewed, now)
					}
				}
				// A later response may reveal an earlier chain's renewal.
				statuses := []appstore.SubscriptionStatus{reminderSignedStatus(t, f, owner, proofs[0]), reminderSignedStatus(t, f, owner, proofs[1])}
				if fault == "renewal" && calls == 2 {
					later := f
					later.now = now.Add(time.Millisecond)
					statuses[0] = reminderSignedStatus(t, later, owner, proofs[0])
				}
				return statuses, nil
			})
			service := reminderVerificationService(t, r, f, client, func() time.Time { return clock })
			proof, err := service.verifyExpiredRetention(context.Background(), r, owner)
			if calls != 2 {
				t.Fatal("did not query every known chain", calls, err)
			}
			if fault == "none" {
				if err != nil || proof.owner != owner || proof.environment != "Sandbox" || proof.revision != before.Revision+2 || !proof.until.Equal(now.Add(30*time.Second)) {
					t.Fatal(proof, err)
				}
			} else if err == nil || proof.owner != "" {
				t.Fatal("unsafe authorization", proof, err)
			}
			after, e := r.CloudAccount(context.Background(), owner, clock)
			if e != nil || after.UsedBytes != before.UsedBytes {
				t.Fatal("verification changed usage", after, e)
			}
			if fault == "renewal" || fault == "concurrent-renewal" {
				if after.State != "active" {
					t.Fatal("renewal lost", after.State, err)
				}
			}
			if fault == "provider" || fault == "unknown-chain" || fault == "missing-chain" {
				if after.Revision != before.Revision {
					t.Fatal("partial proofs committed")
				}
			}
			if fault == "provider" && !errors.Is(err, ErrSubscriptionUnavailable) {
				t.Fatal("provider error escaped", err)
			}
		})
	}
}

func TestMySQLExpiredRetentionNeverAuthorizesBeforeDeadline(t *testing.T) {
	r, owner, now, _, _ := reminderVerificationFixture(t)
	client := &albumStatusStub{}
	service := reminderVerificationService(t, r, newAlbumReceiptFixture(t, now), client, func() time.Time { return now })
	if proof, err := service.verifyExpiredRetention(context.Background(), r, owner); !errors.Is(err, ErrLease) || proof.owner != "" || client.calls != 0 {
		t.Fatal("early cleanup", proof, err)
	}
}
