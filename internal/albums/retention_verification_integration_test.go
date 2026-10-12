package albums

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

type reminderStatusFunc func(context.Context, string) ([]appstore.SubscriptionStatus, error)

func (f reminderStatusFunc) AllSubscriptionStatuses(ctx context.Context, id string) ([]appstore.SubscriptionStatus, error) {
	return f(ctx, id)
}

func reminderVerificationFixture(t *testing.T) (*MySQLUploads, string, time.Time, []VerifiedAlbumSubscription, albumRetentionReminder) {
	t.Helper()
	r, owner, now, v := reminderRepository(t)
	// Apple timestamps have millisecond precision.
	v.ExpiresAt = v.ExpiresAt.Truncate(time.Millisecond)
	v.OriginalTransactionID = "chain-a"
	other := v
	other.OriginalTransactionID, other.TransactionID = "chain-b", "transaction-b"
	other.ExpiresAt = v.ExpiresAt.Add(-24 * time.Hour)
	applyReminderSubscription(t, r, owner, other, now)
	a := applyReminderSubscription(t, r, owner, v, now)
	due := a.RetentionDeadline.AddDate(0, 0, -30)
	return r, owner, due, []VerifiedAlbumSubscription{v, other}, takeReminder(t, r, due, 30)
}
func reminderSignedStatus(t *testing.T, f albumReceiptFixture, owner string, v VerifiedAlbumSubscription) appstore.SubscriptionStatus {
	t.Helper()
	p := f.payload(owner)
	p["originalTransactionId"], p["transactionId"], p["productId"] = v.OriginalTransactionID, v.TransactionID, v.ProductID
	p["expiresDate"], p["purchaseDate"], p["originalPurchaseDate"] = v.ExpiresAt.UnixMilli(), v.ExpiresAt.AddDate(-1, 0, 0).UnixMilli(), v.ExpiresAt.AddDate(-1, 0, 0).UnixMilli()
	status := 2
	if v.Active {
		status = 1
	}
	return appstore.SubscriptionStatus{Status: status, SignedTransaction: f.sign(t, p)}
}
func reminderVerificationService(t *testing.T, r *MySQLUploads, f albumReceiptFixture, client AlbumSubscriptionStatuses, now func() time.Time) *AlbumSubscriptionService {
	t.Helper()
	resolver, err := NewAlbumSubscriptionResolver(f.verifier(), client, "Sandbox", now)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAlbumSubscriptionService(r, resolver, now)
	if err != nil {
		t.Fatal(err)
	}
	return service
}
func TestMySQLRetentionVerificationAllChainsAndRenewal(t *testing.T) {
	for _, renewal := range []bool{false, true} {
		t.Run(fmt.Sprint(renewal), func(t *testing.T) {
			r, owner, now, proofs, claim := reminderVerificationFixture(t)
			before, _ := r.CloudAccount(context.Background(), owner, now)
			f := newAlbumReceiptFixture(t, now)
			if renewal {
				proofs[1].Active = true
				proofs[1].ExpiresAt = now.AddDate(1, 0, 0)
				proofs[1].TransactionID = "renewed-b"
			}
			calls := map[string]int{}
			client := reminderStatusFunc(func(ctx context.Context, id string) ([]appstore.SubscriptionStatus, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 31*time.Second {
					t.Error("missing bounded provider deadline")
				}
				calls[id]++
				// Apple can return all subscription groups for either transaction anchor.
				return []appstore.SubscriptionStatus{reminderSignedStatus(t, f, owner, proofs[0]), reminderSignedStatus(t, f, owner, proofs[1])}, nil
			})
			service := reminderVerificationService(t, r, f, client, func() time.Time { return now })
			eligible, err := service.verifyRetentionReminder(context.Background(), r, claim)
			if err != nil || (eligible > 0) == renewal || len(calls) != 2 || calls["transaction-1"] != 1 || calls["transaction-b"] != 1 {
				t.Fatal("all-chain verification", eligible, err, calls)
			}
			after, err := r.CloudAccount(context.Background(), owner, now)
			if err != nil || after.Revision != before.Revision+2 || after.UsedBytes != 100 {
				t.Fatal("atomic subscription update", after, err)
			}
			var count int
			if err := r.db.QueryRow(`SELECT COUNT(*) FROM album_cloud_subscriptions WHERE owner_id=? AND verified_at=?`, owner, now).Scan(&count); err != nil || count != 2 {
				t.Fatal("missing verified chain", count, err)
			}
			state := "pending"
			if renewal {
				state = "cancelled"
			}
			if err := r.db.QueryRow(`SELECT COUNT(*) FROM album_retention_reminders WHERE reminder_id=? AND state=?`, claim.ID, state).Scan(&count); err != nil || count != 1 {
				t.Fatal("claim state", count, err)
			}
			if renewal && (after.State != "active" || after.QuotaBytes != 50_000_000_000) {
				t.Fatal("renewal lost", after)
			}
		})
	}
}
func TestMySQLRetentionVerificationFailureKeepsSnapshot(t *testing.T) {
	for _, name := range []string{"provider", "empty", "bad-signature", "missing-chain", "unknown-owned-chain", "wrong-owner", "foreign-unrelated-chain", "cancelled-context", "expired-lease", "revision-changed", "renewed-concurrently"} {
		t.Run(name, func(t *testing.T) {
			r, owner, now, proofs, claim := reminderVerificationFixture(t)
			before, _ := r.CloudAccount(context.Background(), owner, now)
			f := newAlbumReceiptFixture(t, now)
			clock := now
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			client := reminderStatusFunc(func(ctx context.Context, id string) ([]appstore.SubscriptionStatus, error) {
				calls++
				i := 0
				if id == "transaction-b" {
					i = 1
				}
				status := reminderSignedStatus(t, f, owner, proofs[i])
				if calls == 2 {
					switch name {
					case "provider":
						return nil, errors.New("provider secret must not escape")
					case "empty":
						return nil, nil
					case "bad-signature":
						status.SignedTransaction = "tampered"
					case "missing-chain":
						return []appstore.SubscriptionStatus{reminderSignedStatus(t, f, owner, proofs[0])}, nil
					case "unknown-owned-chain", "foreign-unrelated-chain":
						unknown := proofs[0]
						unknown.OriginalTransactionID = "new-chain"
						targetOwner := owner
						if name == "foreign-unrelated-chain" {
							targetOwner = uuid.NewString()
						}
						return []appstore.SubscriptionStatus{status, reminderSignedStatus(t, f, targetOwner, unknown)}, nil
					case "wrong-owner":
						status = reminderSignedStatus(t, f, uuid.NewString(), proofs[i])
					case "cancelled-context":
						cancel()
					case "expired-lease":
						clock = claim.Until
					case "revision-changed":
						applyReminderSubscription(t, r, owner, proofs[0], now)
					case "renewed-concurrently":
						renewed := proofs[0]
						renewed.Active = true
						renewed.ExpiresAt = now.AddDate(1, 0, 0)
						renewed.SignedAt = now
						renewed.TransactionID = "concurrent-renewal"
						applyReminderSubscription(t, r, owner, renewed, now)
					}
				}
				return []appstore.SubscriptionStatus{status}, nil
			})
			service := reminderVerificationService(t, r, f, client, func() time.Time { return clock })
			eligible, err := service.verifyRetentionReminder(ctx, r, claim)
			if name == "foreign-unrelated-chain" {
				if err != nil || eligible == 0 {
					t.Fatal("unrelated valid chain blocked", err)
				}
				return
			}
			if err == nil || eligible != 0 || calls != 2 {
				t.Fatal("unsafe verification", eligible, err, calls)
			}
			if name == "unknown-owned-chain" || name == "revision-changed" {
				if !errors.Is(err, ErrSubscriptionConflict) {
					t.Fatal("expected revision conflict", err)
				}
			}
			if name == "expired-lease" || name == "renewed-concurrently" {
				if !errors.Is(err, ErrLease) {
					t.Fatal("expected invalidated lease", err)
				}
			}
			after, e := r.CloudAccount(context.Background(), owner, clock)
			expected := before.Revision
			if name == "revision-changed" || name == "renewed-concurrently" {
				expected++
			}
			if e != nil || after.Revision != expected || after.UsedBytes != 100 {
				t.Fatal("partial mutation", after, e)
			}
			if name == "renewed-concurrently" && after.State != "active" {
				t.Fatal("concurrent renewal overwritten")
			}
			if name == "provider" && err.Error() != "album subscription verification unavailable" {
				t.Fatal("provider failure leaked", err)
			}
		})
	}
}
func TestMySQLRetentionVerificationRollbackAndFencing(t *testing.T) {
	r, owner, now, proofs, claim := reminderVerificationFixture(t)
	revision, anchors, err := r.reminderVerificationSnapshot(context.Background(), claim, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := range proofs {
		proofs[i].SignedAt = now
	}
	proofs[1].Environment = "Production"
	eligible, err := r.commitReminderVerification(context.Background(), claim, revision, anchors, proofs, now)
	if !errors.Is(err, ErrSubscriptionVerification) || eligible != 0 {
		t.Fatal("wrong proof accepted", err)
	}
	a, _ := r.CloudAccount(context.Background(), owner, now)
	if a.Revision != revision {
		t.Fatal("first proof was not rolled back")
	}
	var count int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM album_cloud_subscriptions WHERE owner_id=? AND verified_at=?`, owner, now).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial verified stamp", count, err)
	}
	for _, name := range []string{"token", "owner", "environment", "deadline", "stage"} {
		t.Run(name, func(t *testing.T) {
			forged := claim
			switch name {
			case "token":
				forged.Token = uuid.NewString()
			case "owner":
				forged.Owner = uuid.NewString()
			case "environment":
				forged.Environment = "Production"
			case "deadline":
				forged.Deadline = forged.Deadline.Add(time.Second)
			case "stage":
				forged.Days = 7
			}
			client := &albumStatusStub{}
			service := reminderVerificationService(t, r, newAlbumReceiptFixture(t, now), client, func() time.Time { return now })
			if ok, err := service.verifyRetentionReminder(context.Background(), r, forged); err == nil || ok != 0 || client.calls != 0 {
				t.Fatal("forged claim reached provider", err, client.calls)
			}
		})
	}
}

func TestMySQLRetentionVerificationNeverTruncatesChains(t *testing.T) {
	r, owner, now, _, claim := reminderVerificationFixture(t)
	// The two real anchors plus 127 further known chains exceed the provider-work bound.
	for i := range 127 {
		_, err := r.db.Exec(`INSERT INTO album_cloud_subscriptions(environment,original_transaction_id,owner_id,transaction_id,product_id,quota_bytes,active,expires_at,signed_at,verified_at) SELECT environment,?,owner_id,transaction_id,product_id,quota_bytes,active,expires_at,signed_at,verified_at FROM album_cloud_subscriptions WHERE owner_id=? AND original_transaction_id='chain-a'`, fmt.Sprintf("extra-%d", i), owner)
		if err != nil {
			t.Fatal(err)
		}
	}
	client := &albumStatusStub{}
	service := reminderVerificationService(t, r, newAlbumReceiptFixture(t, now), client, func() time.Time { return now })
	if revision, err := service.verifyRetentionReminder(context.Background(), r, claim); revision != 0 || !errors.Is(err, ErrSubscriptionUnavailable) || client.calls != 0 {
		t.Fatal("truncated incomplete verification", revision, err, client.calls)
	}
}

func TestMySQLRetentionVerificationUsesNewerEvidenceFromEveryResponse(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			r, owner, now, proofs, claim := reminderVerificationFixture(t)
			before, _ := r.CloudAccount(context.Background(), owner, now)
			fixture := newAlbumReceiptFixture(t, now)
			client := reminderStatusFunc(func(_ context.Context, id string) ([]appstore.SubscriptionStatus, error) {
				if id == "transaction-1" {
					return []appstore.SubscriptionStatus{reminderSignedStatus(t, fixture, owner, proofs[0])}, nil
				}
				renewed := proofs[0]
				renewed.Active = true
				renewed.TransactionID = "renewed-while-querying-other-chain"
				renewed.ExpiresAt = now.AddDate(1, 0, 0)
				later := fixture
				if !conflict {
					later.now = now.Add(time.Millisecond)
				}
				return []appstore.SubscriptionStatus{reminderSignedStatus(t, fixture, owner, proofs[1]), reminderSignedStatus(t, later, owner, renewed)}, nil
			})
			service := reminderVerificationService(t, r, fixture, client, func() time.Time { return now })
			revision, err := service.verifyRetentionReminder(context.Background(), r, claim)
			if revision != 0 {
				t.Fatal("renewal visible in another response was ignored")
			}
			after, e := r.CloudAccount(context.Background(), owner, now)
			if e != nil {
				t.Fatal(e)
			}
			if conflict {
				if !errors.Is(err, ErrSubscriptionUnavailable) || after.Revision != before.Revision {
					t.Fatal("contradictory equal-time evidence committed", err)
				}
			} else if err != nil || after.State != "active" {
				t.Fatal("newer renewal was not reconciled", err, after.State)
			}
		})
	}
}
