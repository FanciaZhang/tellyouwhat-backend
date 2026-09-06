package redisstore

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/quota"
)

func prepayRedisJobAttempt(t *testing.T, limiter *QuotaLimiter, now time.Time) quota.JobAttempt {
	t.Helper()
	attempt := quota.JobAttempt{TransactionID: "transaction", DeviceID: "device", ReservationID: "root", ReservedTokens: 300, Number: 1}
	lease, err := limiter.Acquire(context.Background(), quota.Identity{DeviceID: attempt.DeviceID, TransactionID: attempt.TransactionID, IP: "203.0.113.1"}, contracts.OperationMealDecision, attempt.ReservedTokens, attempt.ReservationID, now)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release(attempt.ReservedTokens)
	return attempt
}

func TestJobAttemptRedisAtomicAdmissionAndSettlement(t *testing.T) {
	for _, scope := range []quota.LimitScope{quota.LimitDailyTokens, quota.LimitMonthlyTokens} {
		t.Run(string(scope), func(t *testing.T) {
			client, limiter := newQuotaIntegrationLimiter(t)
			limiter.limits.DailyTokensPerTransaction, limiter.limits.MonthlyTokensPerTransaction = 10_000, 10_000
			if scope == quota.LimitDailyTokens {
				limiter.limits.DailyTokensPerTransaction = 800
			} else {
				limiter.limits.MonthlyTokensPerTransaction = 800
			}
			ctx, now := context.Background(), time.Now()
			attempt := prepayRedisJobAttempt(t, limiter, now)
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
			snapshot, err := limiter.Snapshot(ctx, attempt.TransactionID, now)
			if err != nil || snapshot.DailyUsed != 600 || snapshot.MonthlyUsed != 600 {
				t.Fatalf("duplicate admission changed token counts: %+v err=%v", snapshot, err)
			}
			third := attempt
			third.Number = 3
			if _, err := limiter.ReserveJobAttempt(ctx, third, now); err == nil {
				t.Fatal("third call exceeded quota")
			} else if actual, ok := quota.ExceededScope(err); !ok || actual != scope {
				t.Fatalf("wrong admission failure: %v", err)
			}
			if count, err := client.Exists(ctx, limiter.prefix+"quota:reservation:"+third.TokenReservationID()).Result(); err != nil || count != 0 {
				t.Fatalf("rejected attempt created a reservation: count=%d err=%v", count, err)
			}
			for range 2 {
				if err := limiter.Reconcile(ctx, attempt.TransactionID, attempt.TokenReservationID(), 300, 100, now); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); !errors.Is(err, quota.ErrInvalidReservation) {
				t.Fatalf("settled reservation admitted the same attempt: %v", err)
			}
			if _, err := limiter.ReserveJobAttempt(ctx, third, now); err != nil {
				t.Fatal(err)
			}
			snapshot, err = limiter.Snapshot(ctx, attempt.TransactionID, now)
			if err != nil || snapshot.DailyUsed != 700 || snapshot.MonthlyUsed != 700 {
				t.Fatalf("one settlement changed another attempt: %+v err=%v", snapshot, err)
			}
			for _, id := range []string{attempt.ReservationID, attempt.TokenReservationID(), third.TokenReservationID()} {
				ttl, err := client.TTL(ctx, limiter.prefix+"quota:reservation:"+id).Result()
				if err != nil || ttl <= 0 || ttl > 25*time.Hour {
					t.Fatalf("reservation lost bounded lifetime: %s err=%v", ttl, err)
				}
			}
		})
	}
}

func TestJobAttemptRedisRetriesUseTheirOwnWindows(t *testing.T) {
	_, limiter := newQuotaIntegrationLimiter(t)
	ctx := context.Background()
	before := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
	after := before.Add(2 * time.Second)
	attempt := prepayRedisJobAttempt(t, limiter, before)
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
	previous, err := limiter.Snapshot(ctx, attempt.TransactionID, before)
	if err != nil || previous.DailyUsed != 8 || previous.MonthlyUsed != 8 {
		t.Fatalf("original attempt was settled in the wrong window: %+v err=%v", previous, err)
	}
	snapshot, err := limiter.Snapshot(ctx, attempt.TransactionID, after)
	if err != nil || snapshot.DailyUsed != 300 || snapshot.MonthlyUsed != 300 {
		t.Fatalf("original settlement changed new retry counters: %+v err=%v", snapshot, err)
	}
}

func TestJobAttemptRedisRejectsUnboundAdmission(t *testing.T) {
	for _, scenario := range []string{"missing", "legacy", "device", "transaction", "amount", "attempt_zero", "attempt_excess", "reconciled"} {
		t.Run(scenario, func(t *testing.T) {
			client, limiter := newQuotaIntegrationLimiter(t)
			ctx, now := context.Background(), time.Now()
			attempt := prepayRedisJobAttempt(t, limiter, now)
			switch scenario {
			case "missing":
				if err := client.Del(ctx, limiter.prefix+"quota:reservation:"+attempt.ReservationID).Err(); err != nil {
					t.Fatal(err)
				}
			case "legacy":
				if err := client.Set(ctx, limiter.prefix+"quota:reservation:"+attempt.ReservationID, "1", time.Hour).Err(); err != nil {
					t.Fatal(err)
				}
			case "device":
				attempt.DeviceID = "other"
			case "transaction":
				attempt.TransactionID = "other"
			case "amount":
				attempt.ReservedTokens++
			case "attempt_zero":
				attempt.Number = 0
			case "attempt_excess":
				attempt.Number = quota.MaximumJobAttempts + 1
			case "reconciled":
				if err := limiter.Reconcile(ctx, attempt.TransactionID, attempt.ReservationID, 300, 8, now); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); !errors.Is(err, quota.ErrInvalidReservation) {
				t.Fatalf("invalid admission accepted: %v", err)
			}
		})
	}
}

func TestJobAttemptRedisPrivacyDeletionIncludesRetryReservations(t *testing.T) {
	client, limiter := newQuotaIntegrationLimiter(t)
	otherClient, otherApp := newQuotaIntegrationLimiter(t)
	ctx, now := context.Background(), time.Now()
	attempt := prepayRedisJobAttempt(t, limiter, now)
	other := prepayRedisJobAttempt(t, otherApp, now)
	attempt.Number, other.Number = 2, 2
	if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); err != nil {
		t.Fatal(err)
	}
	if _, err := otherApp.ReserveJobAttempt(ctx, other, now); err != nil {
		t.Fatal(err)
	}
	appID := limiter.prefix[len("platform:") : len(limiter.prefix)-1]
	if err := NewPrivacyCleaner(client, appID).DeletePrincipal(ctx, attestation.Principal{
		KeyID: "key", DeviceID: attempt.DeviceID, TransactionID: attempt.TransactionID,
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{attempt.ReservationID, attempt.TokenReservationID()} {
		if count, err := client.Exists(ctx, limiter.prefix+"quota:reservation:"+id).Result(); err != nil || count != 0 {
			t.Fatalf("deleted owner retained reservation: count=%d err=%v", count, err)
		}
		if count, err := otherClient.Exists(ctx, otherApp.prefix+"quota:reservation:"+id).Result(); err != nil || count != 1 {
			t.Fatalf("deletion crossed application boundary: count=%d err=%v", count, err)
		}
	}
	if _, err := limiter.ReserveJobAttempt(ctx, attempt, now); !errors.Is(err, quota.ErrInvalidReservation) {
		t.Fatalf("deleted reservation was recreated: %v", err)
	}
}

func TestJobAttemptRedisUnavailableFailsClosed(t *testing.T) {
	outage := errors.New("synthetic Redis unavailable")
	client := redis.NewClient(&redis.Options{Addr: "unused.invalid:6379", MaxRetries: -1,
		Dialer: func(context.Context, string, string) (net.Conn, error) { return nil, outage },
	})
	t.Cleanup(func() { _ = client.Close() })
	limiter := NewQuotaLimiter(client, quota.Limits{}, "synthetic")
	id, err := limiter.ReserveJobAttempt(context.Background(), quota.JobAttempt{
		TransactionID: "transaction", DeviceID: "device", ReservationID: "root", ReservedTokens: 300, Number: 1,
	}, time.Now())
	if !errors.Is(err, outage) || id != "" {
		t.Fatalf("unavailable Redis granted admission: id=%q err=%v", id, err)
	}
}
