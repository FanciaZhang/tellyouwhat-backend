package quota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
)

func reserveAttemptFixture(t *testing.T, limiter *MemoryLimiter, now time.Time) JobAttempt {
	t.Helper()
	attempt := JobAttempt{TransactionID: "transaction", DeviceID: "device", ReservationID: "root", ReservedTokens: 300, Number: 1}
	lease, err := limiter.Acquire(context.Background(), Identity{DeviceID: attempt.DeviceID, TransactionID: attempt.TransactionID, IP: "203.0.113.1"}, contracts.OperationMealDecision, attempt.ReservedTokens, attempt.ReservationID, now)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release(attempt.ReservedTokens)
	return attempt
}

func TestMemoryJobAttemptReservationIsAtomicAndIndependentlySettled(t *testing.T) {
	ctx, now := context.Background(), time.Now()
	limiter := NewMemoryLimiter(Limits{DailyTokensPerTransaction: 800, MonthlyTokensPerTransaction: 800})
	attempt := reserveAttemptFixture(t, limiter, now)
	if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); err != nil {
		t.Fatal(err)
	}
	attempt.Number = 2
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); err != nil {
				t.Errorf("concurrent reservation: %v", err)
			}
		}()
	}
	wait.Wait()
	snapshot, _ := limiter.Snapshot(ctx, attempt.TransactionID, now)
	if snapshot.DailyUsed != 600 || snapshot.MonthlyUsed != 600 {
		t.Fatalf("duplicate admission was charged again: %+v", snapshot)
	}
	third := attempt
	third.Number = 3
	if _, err := limiter.ReserveJobAttempt(ctx, third, now); !errors.Is(err, ErrExceeded) {
		t.Fatalf("third attempt exceeded budget: %v", err)
	}
	for range 2 {
		if err := limiter.Reconcile(ctx, attempt.TransactionID, attempt.TokenReservationID(), 300, 100, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("settled attempt reused its reduced charge: %v", err)
	}
	if _, err := limiter.ReserveJobAttempt(ctx, third, now); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = limiter.Snapshot(ctx, attempt.TransactionID, now)
	if snapshot.DailyUsed != 700 || snapshot.MonthlyUsed != 700 {
		t.Fatalf("independent attempt settlement lost another call: %+v", snapshot)
	}
}

func TestMemoryJobAttemptRejectsMissingOrChangedAdmission(t *testing.T) {
	for _, scenario := range []string{"missing", "expired", "device", "transaction", "amount", "attempt_zero", "attempt_excess", "reconciled"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now()
			limiter := NewMemoryLimiter(Limits{})
			attempt := reserveAttemptFixture(t, limiter, now)
			switch scenario {
			case "missing":
				delete(limiter.reservations, attempt.ReservationID)
			case "expired":
				now = now.Add(25 * time.Hour)
			case "device":
				attempt.DeviceID = "other"
			case "transaction":
				attempt.TransactionID = "other"
			case "amount":
				attempt.ReservedTokens++
			case "attempt_zero":
				attempt.Number = 0
			case "attempt_excess":
				attempt.Number = MaximumJobAttempts + 1
			case "reconciled":
				if err := limiter.Reconcile(context.Background(), attempt.TransactionID, attempt.ReservationID, 300, 8, now); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := limiter.ReserveJobAttempt(context.Background(), attempt, now); !errors.Is(err, ErrInvalidReservation) {
				t.Fatalf("invalid admission accepted: %v", err)
			}
		})
	}
}

func TestMemoryJobRetriesUseTheirOwnQuotaWindows(t *testing.T) {
	before := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
	after := before.Add(2 * time.Second)
	ctx := context.Background()
	limiter := NewMemoryLimiter(Limits{DailyTokensPerTransaction: 1_000, MonthlyTokensPerTransaction: 1_000})
	attempt := reserveAttemptFixture(t, limiter, before)
	if _, err := limiter.ReserveJobAttempt(ctx, attempt, after); err != nil {
		t.Fatal(err)
	}
	attempt.Number = 2
	if _, err := limiter.ReserveJobAttempt(ctx, attempt, after); err != nil {
		t.Fatal(err)
	}
	if err := limiter.Reconcile(ctx, attempt.TransactionID, attempt.ReservationID, 300, 8, after); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := limiter.Snapshot(ctx, attempt.TransactionID, after)
	if snapshot.DailyUsed != 300 || snapshot.MonthlyUsed != 300 {
		t.Fatalf("older attempt changed the retry's window: %+v", snapshot)
	}
	if err := limiter.Reconcile(ctx, attempt.TransactionID, attempt.TokenReservationID(), 300, 100, after); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = limiter.Snapshot(ctx, attempt.TransactionID, after)
	if snapshot.DailyUsed != 100 || snapshot.MonthlyUsed != 100 {
		t.Fatalf("retry settled against the wrong window: %+v", snapshot)
	}
}

func TestRoutedJobAttemptUsesFreeRecognitionBudget(t *testing.T) {
	ctx, now := context.Background(), time.Now()
	managed := NewMemoryLimiter(Limits{DailyTokensPerTransaction: 300, MonthlyTokensPerTransaction: 300})
	free := NewMemoryLimiter(Limits{DailyTokensPerTransaction: 1_000, MonthlyTokensPerTransaction: 1_000})
	paidAttempt := reserveAttemptFixture(t, managed, now)
	freeAttempt := paidAttempt
	freeAttempt.TransactionID = "free:key"
	freeAttempt.ReservationID = "free-root"
	lease, err := free.Acquire(ctx, Identity{DeviceID: freeAttempt.DeviceID, TransactionID: freeAttempt.TransactionID, IP: "203.0.113.1"}, contracts.OperationMealDecision, 300, freeAttempt.ReservationID, now)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release(300)
	router := NewRoutedTokenReconciler(managed, free)
	paidAttempt.Number, freeAttempt.Number = 2, 2
	if _, err := router.ReserveJobAttempt(ctx, paidAttempt, now); !errors.Is(err, ErrExceeded) {
		t.Fatalf("paid retry bypassed paid limit: %v", err)
	}
	if _, err := router.ReserveJobAttempt(ctx, freeAttempt, now); err != nil {
		t.Fatalf("free retry incorrectly used paid limit: %v", err)
	}
	if _, err := NewRoutedTokenReconciler(&recordingReconciler{}, nil).ReserveJobAttempt(ctx, paidAttempt, now); !errors.Is(err, ErrAttemptBudgetUnavailable) {
		t.Fatalf("reconcile-only dependency permitted unbudgeted work: %v", err)
	}
}
