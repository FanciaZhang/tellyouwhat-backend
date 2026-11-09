package recognitionquota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMemoryStoreEnforcesThreeConcurrentSessions(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var wait sync.WaitGroup
	var lock sync.Mutex
	succeeded := 0
	exceeded := 0
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.Reserve(context.Background(), request(uuid.NewString()), now)
			lock.Lock()
			defer lock.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrExceeded):
				exceeded++
			default:
				t.Errorf("unexpected reserve error: %v", err)
			}
		}()
	}
	wait.Wait()
	if succeeded != 3 || exceeded != 1 {
		t.Fatalf("expected 3 reservations and one rejection, got success=%d exceeded=%d", succeeded, exceeded)
	}
}

func TestMemoryStoreIsIdempotentAndOnlyCompletedSessionsConsumeTheDay(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sessionID := uuid.NewString()
	first, err := store.Reserve(context.Background(), request(sessionID), now)
	if err != nil || first.Reserved != 1 || first.Remaining != 2 {
		t.Fatalf("unexpected first reservation: %+v err=%v", first, err)
	}
	retry, err := store.Reserve(context.Background(), request(sessionID), now.Add(time.Minute))
	if err != nil || retry.Reserved != 1 || retry.Remaining != 2 {
		t.Fatalf("retry consumed quota: %+v err=%v", retry, err)
	}
	completed, err := store.Complete(context.Background(), "device-1", sessionID, now.Add(2*time.Minute))
	if err != nil || completed.Completed != 1 || completed.Reserved != 0 {
		t.Fatalf("unexpected completion: %+v err=%v", completed, err)
	}
	again, err := store.Complete(context.Background(), "device-1", sessionID, now.Add(3*time.Minute))
	if err != nil || again.Completed != 1 {
		t.Fatalf("completion was not idempotent: %+v err=%v", again, err)
	}
	if err := store.Cancel(context.Background(), "device-1", sessionID, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	afterCancel, _ := store.Snapshot(context.Background(), "device-1", settings(), now.Add(5*time.Minute))
	if afterCancel.Completed != 1 {
		t.Fatalf("completed quota was incorrectly released: %+v", afterCancel)
	}
}

func TestMemoryStoreReleasesCancelledAndExpiredReservations(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	firstID := uuid.NewString()
	if _, err := store.Reserve(context.Background(), request(firstID), now); err != nil {
		t.Fatal(err)
	}
	if err := store.Cancel(context.Background(), "device-1", firstID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	afterCancel, _ := store.Snapshot(context.Background(), "device-1", settings(), now.Add(time.Minute))
	if afterCancel.Reserved != 0 || afterCancel.Remaining != 3 {
		t.Fatalf("cancel did not release reservation: %+v", afterCancel)
	}
	if _, err := store.Reserve(context.Background(), request(uuid.NewString()), now); err != nil {
		t.Fatal(err)
	}
	afterExpiry, _ := store.Snapshot(context.Background(), "device-1", settings(), now.Add(MaximumSessionAge))
	if afterExpiry.Reserved != 0 || afterExpiry.Remaining != 3 {
		t.Fatalf("expired reservation did not release: %+v", afterExpiry)
	}
}

func TestMemoryStoreLocksBusinessWindowSettingsUntilReset(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	first, err := store.Reserve(context.Background(), request(uuid.NewString()), now)
	if err != nil {
		t.Fatal(err)
	}
	changed := request(uuid.NewString())
	changed.Context.BusinessDayStartHour = 15
	second, err := store.Reserve(context.Background(), changed, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !second.ResetAt.Equal(first.ResetAt) {
		t.Fatalf("settings changed the active window: first=%s second=%s", first.ResetAt, second.ResetAt)
	}
	afterReset, err := store.Reserve(context.Background(), changed, first.ResetAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if afterReset.ResetAt.Equal(first.ResetAt) {
		t.Fatalf("new settings did not take effect in next business day: %+v", afterReset)
	}
}

func TestMemoryStoreScopesTheAllowanceToTheAttestedDevice(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for index := range DailySessionLimit {
		value := request(uuid.NewString())
		value.DeviceID = "device-a"
		if _, err := store.Reserve(context.Background(), value, now.Add(time.Duration(index)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	otherDevice := request(uuid.NewString())
	otherDevice.DeviceID = "device-b"
	snapshot, err := store.Reserve(context.Background(), otherDevice, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("a different attested device did not receive its own allowance: %v", err)
	}
	if snapshot.Reserved != 1 || snapshot.Remaining != 2 {
		t.Fatalf("unexpected second-device snapshot: %+v", snapshot)
	}
}

func TestBusinessWindowHandlesDSTByLocalCalendarDay(t *testing.T) {
	start, end, err := BusinessWindow(
		time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC),
		WindowSettings{BusinessDayStartHour: 4, TimeZoneIdentifier: "America/Los_Angeles"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if duration := end.Sub(start); duration != 23*time.Hour {
		t.Fatalf("expected DST business day to be 23 hours, got %s", duration)
	}
}

func request(sessionID string) Request {
	return Request{DeviceID: "device-1", Context: Context{
		SessionID: sessionID, BusinessDayStartHour: 4, TimeZoneIdentifier: "Asia/Shanghai",
	}}
}

func settings() WindowSettings {
	return WindowSettings{BusinessDayStartHour: 4, TimeZoneIdentifier: "Asia/Shanghai"}
}
