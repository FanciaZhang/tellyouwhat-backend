package redisstore

import (
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/quota"
	"sync"
	"testing"
	"time"
)

func TestPreparedJobRedisChargesExecutionOnceAcrossWindows(t *testing.T) {
	_, limiter := newQuotaIntegrationLimiter(t)
	ctx := context.Background()
	before := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
	now := before.Add(2 * time.Second)
	identity := quota.Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
	for _, id := range []string{"abandoned", "cancelled", "executed", "competing"} {
		if err := limiter.PrepareJob(ctx, identity, contracts.OperationDietAnalysis, 700, id, before); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ := limiter.Snapshot(ctx, identity.TransactionID, before)
	if snapshot.DailyUsed != 0 || snapshot.MonthlyUsed != 0 {
		t.Fatalf("preparation spent tokens: %+v", snapshot)
	}
	if err := limiter.Reconcile(ctx, identity.TransactionID, "abandoned", 700, 0, now); !errors.Is(err, quota.ErrInvalidReservation) {
		t.Fatalf("uncharged refund: %v", err)
	}
	attempt := quota.JobAttempt{DeviceID: identity.DeviceID, TransactionID: identity.TransactionID, ReservationID: "executed", ReservedTokens: 700, Number: 1}
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
	if snapshot.DailyUsed != 700 || snapshot.MonthlyUsed != 700 {
		t.Fatalf("execution charged wrong window or duplicated charge: %+v", snapshot)
	}
	competing := attempt
	competing.ReservationID = "competing"
	if _, err := limiter.ReserveJobAttempt(ctx, competing, now); !errors.Is(err, quota.ErrExceeded) {
		t.Fatalf("current budget bypassed: %v", err)
	}
	for range 2 {
		if err := limiter.Reconcile(ctx, identity.TransactionID, "executed", 700, 100, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); !errors.Is(err, quota.ErrInvalidReservation) {
		t.Fatalf("settled execution replay: %v", err)
	}
	if _, err := limiter.ReserveJobAttempt(ctx, competing, now); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = limiter.Snapshot(ctx, identity.TransactionID, now)
	if snapshot.DailyUsed != 800 || snapshot.MonthlyUsed != 800 {
		t.Fatalf("settlement lost unrelated usage: %+v", snapshot)
	}
}

func TestPreparedJobRedisRetainsRateLimitsAndBinding(t *testing.T) {
	_, limiter := newQuotaIntegrationLimiter(t)
	limiter.limits.RequestsPerMinutePerDevice = 1
	ctx, now := context.Background(), time.Now()
	identity := quota.Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
	for range 2 {
		if err := limiter.PrepareJob(ctx, identity, contracts.OperationDietAnalysis, 700, "root", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := limiter.PrepareJob(ctx, identity, contracts.OperationDietAnalysis, 700, "other", now); !errors.Is(err, quota.ErrExceeded) {
		t.Fatalf("rate limit bypassed: %v", err)
	}
	if err := limiter.PrepareJob(ctx, identity, contracts.OperationDietAnalysis, 600, "root", now); !errors.Is(err, quota.ErrInvalidReservation) {
		t.Fatalf("amount changed: %v", err)
	}
	if _, err := limiter.Acquire(ctx, identity, contracts.OperationDietAnalysis, 700, "root", now); !errors.Is(err, quota.ErrInvalidReservation) {
		t.Fatalf("synchronous request reused unpaid job: %v", err)
	}
}

func TestLegacyUnclaimedJobRepairIsBoundedIdempotentAndClaimSafe(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "unclaimed", true: "claimed"}[started], func(t *testing.T) {
			_, limiter := newQuotaIntegrationLimiter(t)
			ctx, now := context.Background(), time.Now()
			identity := quota.Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
			lease, err := limiter.Acquire(ctx, identity, contracts.OperationDietAnalysis, 700, "legacy", now)
			if err != nil {
				t.Fatal(err)
			}
			lease.Release(700)
			attempt := quota.JobAttempt{DeviceID: identity.DeviceID, TransactionID: identity.TransactionID, ReservationID: "legacy", ReservedTokens: 700, Number: 1}
			if started {
				if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); err != nil {
					t.Fatal(err)
				}
			}
			expected := 700
			if started {
				expected = 0
			}
			n, err := limiter.DeferLegacyJob(ctx, "legacy", false)
			if err != nil || n != expected {
				t.Fatalf("preview: tokens=%d err=%v", n, err)
			}
			snapshot, _ := limiter.Snapshot(ctx, identity.TransactionID, now)
			if snapshot.DailyUsed != 700 {
				t.Fatal("preview mutated quota")
			}
			n, err = limiter.DeferLegacyJob(ctx, "legacy", true)
			if err != nil || n != expected {
				t.Fatalf("repair: tokens=%d err=%v", n, err)
			}
			n, err = limiter.DeferLegacyJob(ctx, "legacy", true)
			if err != nil || n != 0 {
				t.Fatalf("repair double refunded: %d %v", n, err)
			}
			snapshot, _ = limiter.Snapshot(ctx, identity.TransactionID, now)
			if snapshot.DailyUsed != 700-expected || snapshot.MonthlyUsed != 700-expected {
				t.Fatalf("repair counters: %+v", snapshot)
			}
			if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); err != nil {
				t.Fatal(err)
			}
			snapshot, _ = limiter.Snapshot(ctx, identity.TransactionID, now)
			if snapshot.DailyUsed != 700 {
				t.Fatal("late upload bypassed execution budget")
			}
		})
	}
}

func TestPreparedRedisProtectionDeferralReleasesAndRechecksEachAttempt(t *testing.T) {
	_, limiter := newQuotaIntegrationLimiter(t)
	ctx, now := context.Background(), time.Now()
	identity := quota.Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
	if err := limiter.PrepareJob(ctx, identity, contracts.OperationDietAnalysis, 700, "root", now); err != nil {
		t.Fatal(err)
	}
	attempt := quota.JobAttempt{DeviceID: identity.DeviceID, TransactionID: identity.TransactionID, ReservationID: "root", ReservedTokens: 700, Number: 1}
	for _, number := range []int{1, 2} {
		attempt.Number = number
		if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := limiter.DeferJobAttempt(ctx, attempt, now); err != nil {
				t.Fatal(err)
			}
		}
		snapshot, _ := limiter.Snapshot(ctx, identity.TransactionID, now)
		if snapshot.DailyUsed != 0 {
			t.Fatalf("no-call attempt %d consumed quota: %+v", number, snapshot)
		}
		if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); err != nil {
			t.Fatal(err)
		}
		snapshot, _ = limiter.Snapshot(ctx, identity.TransactionID, now)
		if snapshot.DailyUsed != 700 {
			t.Fatalf("resumption bypassed charge: %+v", snapshot)
		}
		if err := limiter.DeferJobAttempt(ctx, attempt, now); err != nil {
			t.Fatal(err)
		}
	}
}
