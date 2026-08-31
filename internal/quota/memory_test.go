package quota

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
)

func TestLimiterEnforcesIPDeviceOperationTokenAndConcurrencyDimensions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	limiter := NewMemoryLimiter(Limits{
		RequestsPerMinutePerIP:        2,
		RequestsPerMinutePerDevice:    2,
		RequestsPerMinutePerOperation: 2,
		DailyTokensPerTransaction:     100,
		MonthlyTokensPerTransaction:   200,
		MaxConcurrentPerDevice:        1,
	})
	identity := Identity{DeviceID: "device-1", TransactionID: "transaction-1", IP: "203.0.113.1"}

	lease, err := limiter.Acquire(context.Background(), identity, contracts.OperationMealDecision, 60, "", now)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := limiter.Acquire(context.Background(), identity, contracts.OperationMealDecision, 10, "", now); !errors.Is(err, ErrExceeded) {
		t.Fatalf("concurrent request should be rejected: %v", err)
	}
	lease.Release(40)
	second, err := limiter.Acquire(context.Background(), identity, contracts.OperationMealDecision, 60, "", now)
	if err != nil {
		t.Fatalf("actual usage should replace reservation: %v", err)
	}
	second.Release(60)
	if _, err := limiter.Acquire(context.Background(), identity, contracts.OperationMealDecision, 1, "", now); !errors.Is(err, ErrExceeded) {
		t.Fatalf("minute or daily boundary should be enforced: %v", err)
	}
}

func TestStableReservationMakesLostResponseRetryQuotaIdempotent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	limiter := NewMemoryLimiter(Limits{
		RequestsPerMinutePerDevice: 1,
		DailyTokensPerTransaction:  100,
		MaxConcurrentPerDevice:     1,
	})
	identity := Identity{DeviceID: "device-1", TransactionID: "transaction-1", IP: "203.0.113.1"}
	first, err := limiter.Acquire(context.Background(), identity, contracts.OperationMealDecision, 60, "capability-request-1", now)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	first.Release(60)
	replay, err := limiter.Acquire(context.Background(), identity, contracts.OperationMealDecision, 60, "capability-request-1", now)
	if err != nil {
		t.Fatalf("idempotent acquire: %v", err)
	}
	replay.Release(0)
	if _, err := limiter.Acquire(context.Background(), identity, contracts.OperationMealDecision, 41, "capability-request-2", now.Add(time.Minute)); !errors.Is(err, ErrExceeded) {
		t.Fatalf("replay changed the original 60-token reservation: %v", err)
	}
}

func TestSnapshotReportsReconciledDailyAndMonthlyUsage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	limiter := NewMemoryLimiter(Limits{
		DailyTokensPerTransaction:   1_000,
		MonthlyTokensPerTransaction: 10_000,
	})
	lease, err := limiter.Acquire(
		context.Background(),
		Identity{DeviceID: "device-1", TransactionID: "transaction-1", IP: "203.0.113.1"},
		contracts.OperationMealDecision,
		100,
		"request-1",
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release(75)
	snapshot, err := limiter.Snapshot(context.Background(), "transaction-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DailyUsed != 75 || snapshot.MonthlyUsed != 75 ||
		snapshot.DailyLimit != 1_000 || snapshot.MonthlyLimit != 10_000 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
}

func TestLimiterReportsTheExceededSafetyWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	limiter := NewMemoryLimiter(Limits{
		DailyTokensPerTransaction:   50,
		MonthlyTokensPerTransaction: 1_000,
	})
	_, err := limiter.Acquire(
		context.Background(),
		Identity{DeviceID: "device-1", TransactionID: "transaction-1", IP: "203.0.113.1"},
		contracts.OperationMealDecision,
		51,
		"",
		now,
	)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected quota sentinel, got %v", err)
	}
	if scope, ok := ExceededScope(err); !ok || scope != LimitDailyTokens {
		t.Fatalf("expected daily token scope, got %q ok=%t", scope, ok)
	}
}
