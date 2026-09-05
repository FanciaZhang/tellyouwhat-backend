package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/quota"
)

func TestWorkerReconcilesTheOriginalQuotaWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	reservedAt := now.Truncate(24 * time.Hour).Add(-time.Second)
	limiter := quota.NewMemoryLimiter(quota.Limits{
		DailyTokensPerTransaction: 100_000, MonthlyTokensPerTransaction: 1_000_000,
	})
	principal := attestation.Principal{KeyID: "key-1", DeviceID: "device-1", TransactionID: "transaction-1"}
	identity := quota.Identity{DeviceID: principal.DeviceID, TransactionID: principal.TransactionID, IP: "203.0.113.1"}
	request := jobRequest()
	digest := "job-body-digest"
	reservationID := contracts.BodySHA256([]byte("job-capability\n" + principal.KeyID + "\n" + request.RequestID + "\n" + digest))
	reserved := contracts.ReservationTokens(request)
	lease, err := limiter.Acquire(ctx, identity, request.Operation, reserved, reservationID, reservedAt)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release(reserved)

	current, err := limiter.Acquire(ctx, identity, request.Operation, 250, "", now)
	if err != nil {
		t.Fatal(err)
	}
	current.Release(250)

	store := NewMemoryStore()
	service := NewService(store, func() time.Time { return now })
	job, err := service.Enqueue(ctx, principal, request, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewWorker(store, fixedJobProvider{}, limiter).Process(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := limiter.Snapshot(ctx, principal.TransactionID, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DailyUsed != 250 {
		t.Fatalf("older job changed today's unrelated 250-token usage to %d", snapshot.DailyUsed)
	}
	wantMonth := 250
	if now.Format("200601") == reservedAt.Format("200601") {
		wantMonth += 8
	}
	if snapshot.MonthlyUsed != wantMonth {
		t.Fatalf("monthly usage = %d, want %d", snapshot.MonthlyUsed, wantMonth)
	}
}
