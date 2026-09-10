package quota

import (
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/contracts"
	"sync"
	"testing"
	"time"
)

func TestPreparedJobsChargeOnlyExecutionAndRecheckCapacity(t *testing.T) {
	ctx, now := context.Background(), time.Now().UTC()
	limiter := NewMemoryLimiter(Limits{DailyTokensPerTransaction: 1000, MonthlyTokensPerTransaction: 1000})
	identity := Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
	for _, id := range []string{"abandoned", "cancelled", "executed", "competing"} {
		if err := limiter.PrepareJob(ctx, identity, contracts.OperationDietAnalysis, 700, id, now); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ := limiter.Snapshot(ctx, identity.TransactionID, now)
	if snapshot.DailyUsed != 0 || snapshot.MonthlyUsed != 0 {
		t.Fatalf("unused capabilities spent tokens: %+v", snapshot)
	}
	if err := limiter.Reconcile(ctx, identity.TransactionID, "abandoned", 700, 0, now); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("uncharged reservation could subtract usage: %v", err)
	}
	attempt := JobAttempt{DeviceID: identity.DeviceID, TransactionID: identity.TransactionID, ReservationID: "executed", ReservedTokens: 700, Number: 1}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	snapshot, _ = limiter.Snapshot(ctx, identity.TransactionID, now)
	if snapshot.DailyUsed != 700 {
		t.Fatalf("execution was not charged exactly once: %+v", snapshot)
	}
	competing := attempt
	competing.ReservationID = "competing"
	if _, err := limiter.ReserveJobAttempt(ctx, competing, now); !errors.Is(err, ErrExceeded) {
		t.Fatalf("execution bypassed current quota: %v", err)
	}
	for range 2 {
		if err := limiter.Reconcile(ctx, identity.TransactionID, "executed", 700, 100, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("settled execution replay: %v", err)
	}
	if _, err := limiter.ReserveJobAttempt(ctx, competing, now); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = limiter.Snapshot(ctx, identity.TransactionID, now)
	if snapshot.DailyUsed != 800 || snapshot.MonthlyUsed != 800 {
		t.Fatalf("settlement changed unrelated usage: %+v", snapshot)
	}
}

func TestPreparedJobBillsExecutionWindowAndPreservesRateLimit(t *testing.T) {
	ctx := context.Background()
	before := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
	after := before.Add(2 * time.Second)
	limiter := NewMemoryLimiter(Limits{RequestsPerMinutePerDevice: 1, DailyTokensPerTransaction: 1000, MonthlyTokensPerTransaction: 1000})
	identity := Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
	if err := limiter.PrepareJob(ctx, identity, contracts.OperationDietAnalysis, 700, "root", before); err != nil {
		t.Fatal(err)
	}
	if err := limiter.PrepareJob(ctx, identity, contracts.OperationDietAnalysis, 700, "root", before); err != nil {
		t.Fatal(err)
	}
	if err := limiter.PrepareJob(ctx, identity, contracts.OperationDietAnalysis, 700, "other", before); !errors.Is(err, ErrExceeded) {
		t.Fatalf("preparation bypassed rate limit: %v", err)
	}
	attempt := JobAttempt{DeviceID: identity.DeviceID, TransactionID: identity.TransactionID, ReservationID: "root", ReservedTokens: 700, Number: 1}
	if _, err := limiter.ReserveJobAttempt(ctx, attempt, after); err != nil {
		t.Fatal(err)
	}
	if err := limiter.Reconcile(ctx, identity.TransactionID, "root", 700, 50, after); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := limiter.Snapshot(ctx, identity.TransactionID, after)
	if snapshot.DailyUsed != 50 || snapshot.MonthlyUsed != 50 {
		t.Fatalf("execution charged wrong window: %+v", snapshot)
	}
}
