package quota

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
)

func TestLateLeaseReleasePreservesTheNextBillingWindow(t *testing.T) {
	ctx := context.Background()
	before := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)
	after := before.Add(2 * time.Second)
	limiter := NewMemoryLimiter(Limits{DailyTokensPerTransaction: 1_000, MonthlyTokensPerTransaction: 10_000})
	identity := Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
	old, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, 100, "", before)
	if err != nil {
		t.Fatal(err)
	}
	current, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, 250, "", after)
	if err != nil {
		t.Fatal(err)
	}
	current.Release(250)
	old.Release(8)
	snapshot, err := limiter.Snapshot(ctx, identity.TransactionID, after)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DailyUsed != 250 || snapshot.MonthlyUsed != 250 {
		t.Fatalf("late release erased the next day/month usage: daily=%d monthly=%d", snapshot.DailyUsed, snapshot.MonthlyUsed)
	}
}

func TestReservationReconciliationIsBoundAndIdempotent(t *testing.T) {
	for _, before := range []time.Time{
		time.Date(2026, 9, 6, 23, 59, 59, 0, time.UTC),
		time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC),
	} {
		t.Run(before.Format("20060102"), func(t *testing.T) {
			ctx := context.Background()
			limiter := NewMemoryLimiter(Limits{DailyTokensPerTransaction: 1_000, MonthlyTokensPerTransaction: 10_000})
			identity := Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
			lease, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, 600, "job-reservation", before)
			if err != nil {
				t.Fatal(err)
			}
			lease.Release(600)
			after := before.Add(2 * time.Second)
			current, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, 250, "", after)
			if err != nil {
				t.Fatal(err)
			}
			current.Release(250)
			for _, mismatch := range []struct {
				owner    string
				reserved int
			}{{"other-transaction", 600}, {identity.TransactionID, 599}} {
				if err := limiter.Reconcile(ctx, mismatch.owner, "job-reservation", mismatch.reserved, 8, after); !errors.Is(err, ErrInvalidReservation) {
					t.Fatalf("unbound adjustment was accepted: %v", err)
				}
			}
			for range 3 {
				if err := limiter.Reconcile(ctx, identity.TransactionID, "job-reservation", 600, 8, after); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := limiter.Snapshot(ctx, identity.TransactionID, after)
			if err != nil {
				t.Fatal(err)
			}
			wantMonth := 250
			if before.Month() == after.Month() {
				wantMonth += 8
			}
			if snapshot.DailyUsed != 250 || snapshot.MonthlyUsed != wantMonth {
				t.Fatalf("original-window adjustment damaged current usage: %+v", snapshot)
			}
		})
	}
}

func TestReservationReplayCannotChangeItsAccountingIdentity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	limiter := NewMemoryLimiter(Limits{DailyTokensPerTransaction: 1_000})
	identity := Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
	lease, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, 600, "reservation", now)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release(600)
	other := identity
	other.TransactionID = "other-transaction"
	if _, err := limiter.Acquire(ctx, other, contracts.OperationMealDecision, 600, "reservation", now); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("reservation replay changed its owner: %v", err)
	}
	if _, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, 599, "reservation", now); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("reservation replay changed its amount: %v", err)
	}
	if err := limiter.Reconcile(ctx, identity.TransactionID, "missing", 600, 8, now); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("missing reservation adjusted current usage: %v", err)
	}
	if err := limiter.Reconcile(ctx, identity.TransactionID, "reservation", 600, 8, now.Add(25*time.Hour)); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("expired reservation adjusted current usage: %v", err)
	}
	snapshot, err := limiter.Snapshot(ctx, identity.TransactionID, now)
	if err != nil || snapshot.DailyUsed != 600 {
		t.Fatalf("rejected adjustments changed original reservation: %+v err=%v", snapshot, err)
	}
}
