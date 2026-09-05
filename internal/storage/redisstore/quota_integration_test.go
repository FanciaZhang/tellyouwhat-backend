package redisstore

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/recognitionquota"
)

func newQuotaIntegrationLimiter(t *testing.T) (*redis.Client, *QuotaLimiter) {
	t.Helper()
	redisURL := strings.TrimSpace(os.Getenv("REDIS_TEST_URL"))
	if redisURL == "" {
		t.Skip("set REDIS_TEST_URL to run Redis integration tests")
	}
	ctx := context.Background()
	client, err := Open(ctx, redisURL)
	if err != nil {
		t.Fatal(err)
	}
	limiter := NewQuotaLimiter(client, quota.Limits{
		DailyTokensPerTransaction: 1_000, MonthlyTokensPerTransaction: 10_000,
	}, "quota-integration-"+uuid.NewString())
	t.Cleanup(func() {
		defer client.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, err := client.Scan(cleanup, cursor, limiter.prefix+"*", 100).Result()
			if err != nil {
				t.Errorf("clean up isolated quota keys: %v", err)
				return
			}
			if len(keys) > 0 {
				if err := client.Del(cleanup, keys...).Err(); err != nil {
					t.Errorf("remove isolated quota keys: %v", err)
					return
				}
			}
			cursor = next
			if cursor == 0 {
				return
			}
		}
	})
	return client, limiter
}

func TestQuotaReservationRedisReconcilesOriginalWindowsOnce(t *testing.T) {
	for _, before := range []time.Time{
		time.Date(2026, 9, 6, 23, 59, 59, 0, time.UTC),
		time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC),
	} {
		t.Run(before.Format("20060102"), func(t *testing.T) {
			client, limiter := newQuotaIntegrationLimiter(t)
			ctx := context.Background()
			identity := quota.Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
			lease, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, 600, "reservation", before)
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
			var wait sync.WaitGroup
			for range 8 {
				wait.Add(1)
				go func() {
					defer wait.Done()
					if err := limiter.Reconcile(ctx, identity.TransactionID, "reservation", 600, 8, after); err != nil {
						t.Errorf("concurrent reconciliation: %v", err)
					}
				}()
			}
			wait.Wait()
			previous, err := limiter.Snapshot(ctx, identity.TransactionID, before)
			if err != nil || previous.DailyUsed != 8 {
				t.Fatalf("original day was not reconciled once: %+v err=%v", previous, err)
			}
			snapshot, err := limiter.Snapshot(ctx, identity.TransactionID, after)
			wantMonth := 250
			if before.Month() == after.Month() {
				wantMonth += 8
			}
			if err != nil || snapshot.DailyUsed != 250 || snapshot.MonthlyUsed != wantMonth {
				t.Fatalf("current day/month changed: %+v err=%v", snapshot, err)
			}
			ttl, err := client.TTL(ctx, limiter.prefix+"quota:reservation:reservation").Result()
			if err != nil || ttl <= 0 || ttl > 25*time.Hour {
				t.Fatalf("reconciled reservation lost bounded expiry: %s err=%v", ttl, err)
			}
		})
	}
}

func TestQuotaRedisRejectsUnboundAdjustments(t *testing.T) {
	for _, scenario := range []string{"legacy", "expired", "owner", "amount"} {
		t.Run(scenario, func(t *testing.T) {
			client, limiter := newQuotaIntegrationLimiter(t)
			ctx := context.Background()
			now := time.Now().UTC()
			identity := quota.Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
			lease, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, 600, "reservation", now)
			if err != nil {
				t.Fatal(err)
			}
			lease.Release(600)
			owner, reserved := identity.TransactionID, 600
			key := limiter.prefix + "quota:reservation:reservation"
			switch scenario {
			case "legacy":
				if err := client.Set(ctx, key, "1", time.Hour).Err(); err != nil {
					t.Fatal(err)
				}
				replay, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, 600, "reservation", now)
				if err != nil {
					t.Fatalf("legacy replay lost its existing reservation: %v", err)
				}
				replay.Release(0)
			case "expired":
				if err := client.PExpire(ctx, key, 0).Err(); err != nil {
					t.Fatal(err)
				}
			case "owner":
				owner = "other-transaction"
				other := identity
				other.TransactionID = owner
				if _, err := limiter.Acquire(ctx, other, contracts.OperationMealDecision, 600, "reservation", now); !errors.Is(err, quota.ErrInvalidReservation) {
					t.Fatalf("replay changed reservation owner: %v", err)
				}
			case "amount":
				reserved = 599
				if _, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, reserved, "reservation", now); !errors.Is(err, quota.ErrInvalidReservation) {
					t.Fatalf("replay changed reservation amount: %v", err)
				}
			}
			if err := limiter.Reconcile(ctx, owner, "reservation", reserved, 8, now); !errors.Is(err, quota.ErrInvalidReservation) {
				t.Fatalf("unbound adjustment was accepted: %v", err)
			}
			snapshot, err := limiter.Snapshot(ctx, identity.TransactionID, now)
			if err != nil || snapshot.DailyUsed != 600 || snapshot.MonthlyUsed != 600 {
				t.Fatalf("unbound adjustment changed charged tokens: %+v err=%v", snapshot, err)
			}
		})
	}
}

func TestQuotaRedisLateReleaseDoesNotRecreateExpiredCounters(t *testing.T) {
	client, limiter := newQuotaIntegrationLimiter(t)
	ctx := context.Background()
	now := time.Now().UTC()
	identity := quota.Identity{DeviceID: "device", TransactionID: "transaction", IP: "203.0.113.1"}
	lease, err := limiter.Acquire(ctx, identity, contracts.OperationMealDecision, 600, "", now)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		limiter.prefix + "quota:day:" + now.Format("20060102") + ":" + identity.TransactionID,
		limiter.prefix + "quota:month:" + now.Format("200601") + ":" + identity.TransactionID,
	}
	for _, key := range keys {
		if err := client.PExpire(ctx, key, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	lease.Release(8)
	if count, err := client.Exists(ctx, keys...).Result(); err != nil || count != 0 {
		t.Fatalf("late release recreated expired counters: count=%d err=%v", count, err)
	}
}

func TestQuotaRedisPrivacyDeletionRemovesOwnedReservationsAndRecognitionOnly(t *testing.T) {
	client, limiter := newQuotaIntegrationLimiter(t)
	_, otherApp := newQuotaIntegrationLimiter(t)
	ctx := context.Background()
	now := time.Now().UTC()
	appID := strings.TrimSuffix(strings.TrimPrefix(limiter.prefix, "platform:"), ":")
	otherAppID := strings.TrimSuffix(strings.TrimPrefix(otherApp.prefix, "platform:"), ":")
	principal := attestation.Principal{KeyID: "key", DeviceID: "device", TransactionID: "transaction"}
	reserve := func(target *QuotaLimiter, owner, id string) {
		t.Helper()
		deviceID := principal.DeviceID
		if owner == "other-owner" {
			deviceID = "other-device"
		}
		lease, err := target.Acquire(ctx, quota.Identity{
			DeviceID: deviceID, TransactionID: owner, IP: "203.0.113.1",
		}, contracts.OperationMealDecision, 100, id, now)
		if err != nil {
			t.Fatal(err)
		}
		lease.Release(100)
	}
	reserve(limiter, principal.TransactionID, "paid")
	reserve(limiter, principal.KeyID, "development")
	reserve(limiter, quota.FreeRecognitionTransactionPrefix+principal.KeyID, "free")
	reserve(limiter, "previous-transaction", "previous")
	reserve(limiter, "other-owner", "other-owner")
	reserve(otherApp, principal.TransactionID, "paid")
	for _, id := range []string{appID, otherAppID} {
		key := appRecognitionWindowKey(id, principal.DeviceID)
		t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })
		if _, err := NewRecognitionQuotaStore(client, id).Reserve(ctx, recognitionquota.Request{
			DeviceID: principal.DeviceID,
			Context:  recognitionquota.Context{SessionID: uuid.NewString(), BusinessDayStartHour: 4, TimeZoneIdentifier: "Asia/Shanghai"},
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := NewPrivacyCleaner(client, appID).DeletePrincipal(ctx, principal); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"paid", "development", "free", "previous"} {
		if count, err := client.Exists(ctx, limiter.prefix+"quota:reservation:"+id).Result(); err != nil || count != 0 {
			t.Fatalf("owned reservation survived deletion: %s count=%d err=%v", id, count, err)
		}
	}
	for _, owner := range []string{principal.TransactionID, principal.KeyID, quota.FreeRecognitionTransactionPrefix + principal.KeyID, "previous-transaction"} {
		snapshot, err := limiter.Snapshot(ctx, owner, now)
		if err != nil || snapshot.DailyUsed != 0 || snapshot.MonthlyUsed != 0 {
			t.Fatalf("owned billing counters survived deletion: %+v err=%v", snapshot, err)
		}
	}
	for _, key := range []string{
		limiter.prefix + "quota:reservation:other-owner",
		otherApp.prefix + "quota:reservation:paid",
		appRecognitionWindowKey(otherAppID, principal.DeviceID),
	} {
		if count, err := client.Exists(ctx, key).Result(); err != nil || count != 1 {
			t.Fatalf("another identity or app lost its data: count=%d err=%v", count, err)
		}
	}
	if count, err := client.Exists(ctx, appRecognitionWindowKey(appID, principal.DeviceID)).Result(); err != nil || count != 0 {
		t.Fatalf("owned free recognition window survived deletion: count=%d err=%v", count, err)
	}
}
