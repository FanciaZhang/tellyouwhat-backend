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
	"github.com/tellyouwhat/backend/internal/recognitionquota"
)

func TestRecognitionQuotaRedisIntegration(t *testing.T) {
	redisURL := strings.TrimSpace(os.Getenv("REDIS_TEST_URL"))
	if redisURL == "" {
		t.Skip("set REDIS_TEST_URL to run Redis integration tests")
	}
	ctx := context.Background()
	client, err := Open(ctx, redisURL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	deviceID := "integration-" + uuid.NewString()
	defer client.Del(ctx, recognitionWindowKey(deviceID))
	store := NewRecognitionQuotaStore(client)
	now := time.Now().UTC()
	var wait sync.WaitGroup
	var lock sync.Mutex
	succeeded := 0
	exceeded := 0
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, reserveErr := store.Reserve(ctx, recognitionquota.Request{
				DeviceID: deviceID,
				Context: recognitionquota.Context{
					SessionID: uuid.NewString(), BusinessDayStartHour: 4, TimeZoneIdentifier: "Asia/Shanghai",
				},
			}, now)
			lock.Lock()
			defer lock.Unlock()
			switch {
			case reserveErr == nil:
				succeeded++
			case errors.Is(reserveErr, recognitionquota.ErrExceeded):
				exceeded++
			default:
				t.Errorf("unexpected Redis reservation error: %v", reserveErr)
			}
		}()
	}
	wait.Wait()
	if succeeded != 3 || exceeded != 1 {
		t.Fatalf("expected three Redis reservations and one rejection, got success=%d exceeded=%d", succeeded, exceeded)
	}
}
