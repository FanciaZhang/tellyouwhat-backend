package redisstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/recognitionquota"
)

func TestLiveQuotaChangesKeepExistingCountersAndReleases(t *testing.T) {
	_, limiter := newQuotaIntegrationLimiter(t)
	ctx := context.Background()
	now := time.Now().UTC()
	limits := quota.Limits{DailyTokensPerTransaction: 1000, MonthlyTokensPerTransaction: 10000}
	limiter.ResolveLimits = func(context.Context) (quota.Limits, error) { return limits, nil }
	id := quota.Identity{DeviceID: uuid.NewString(), TransactionID: uuid.NewString(), IP: "127.0.0.1"}
	lease, err := limiter.Acquire(ctx, id, contracts.OperationMealTextCapture, 800, "", now)
	if err != nil {
		t.Fatal(err)
	}
	limits.DailyTokensPerTransaction = 500
	if _, err = limiter.Acquire(ctx, id, contracts.OperationMealTextCapture, 1, "", now); !errors.Is(err, quota.ErrExceeded) {
		t.Fatalf("lowered quota accepted: %v", err)
	}
	lease.Release(100)
	snapshot, err := limiter.Snapshot(ctx, id.TransactionID, now)
	if err != nil || snapshot.DailyUsed != 100 || snapshot.DailyLimit != 500 {
		t.Fatalf("original counters changed: %+v %v", snapshot, err)
	}
	lease, err = limiter.Acquire(ctx, id, contracts.OperationMealTextCapture, 400, "", now)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release(200)
	limiter.ResolveLimits = func(context.Context) (quota.Limits, error) { return quota.Limits{}, errors.New("policy offline") }
	if _, err = limiter.Acquire(ctx, id, contracts.OperationMealTextCapture, 1, "", now); err == nil {
		t.Fatal("policy failure admitted request")
	}
}
func TestLiveFreeRecognitionLimitPreservesCompletedAndReservedSessions(t *testing.T) {
	client, limiter := newQuotaIntegrationLimiter(t)
	ctx := context.Background()
	s := NewRecognitionQuotaStore(client, limiter.prefix)
	limit := 3
	s.ResolveDailyLimit = func(context.Context) (int, error) { return limit, nil }
	now := time.Now().UTC()
	device := uuid.NewString()
	t.Cleanup(func() { client.Del(ctx, s.recognitionWindowKey(device)) })
	settings := recognitionquota.WindowSettings{TimeZoneIdentifier: "Asia/Shanghai", BusinessDayStartHour: 4}
	request := func() recognitionquota.Request {
		return recognitionquota.Request{DeviceID: device, Context: recognitionquota.Context{SessionID: uuid.NewString(), TimeZoneIdentifier: settings.TimeZoneIdentifier, BusinessDayStartHour: settings.BusinessDayStartHour}}
	}
	first, second := request(), request()
	if _, err := s.Reserve(ctx, first, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(ctx, device, first.Context.SessionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, second, now); err != nil {
		t.Fatal(err)
	}
	limit = 1
	if _, err := s.Reserve(ctx, request(), now); !errors.Is(err, recognitionquota.ErrExceeded) {
		t.Fatal("lowered session limit ignored")
	}
	if snap, err := s.Complete(ctx, device, second.Context.SessionID, now); err != nil || snap.Completed != 2 || snap.Remaining != 0 {
		t.Fatalf("existing session broken: %+v %v", snap, err)
	}
	limit = 4
	if snap, err := s.Snapshot(ctx, device, settings, now); err != nil || snap.Remaining != 2 {
		t.Fatalf("used sessions reset: %+v %v", snap, err)
	}
}
