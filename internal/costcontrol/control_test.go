package costcontrol

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestProjectBudgetReservesBeforeCallsAndReconcilesKnownCost(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	controller, err := New(store, Limits{MonthlyBudgetNanos: 100, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	first, err := controller.Reserve(context.Background(), "health", "meal_photo_capture", "ark", 70)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reserve(context.Background(), "journal", "journal.organize", "ark", 40); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected shared project budget rejection, got %v", err)
	}
	if err := first.Settle(context.Background(), 20, true); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reserve(context.Background(), "journal", "journal.organize", "ark", 40); err != nil {
		t.Fatalf("known cost did not release conservative reserve: %v", err)
	}
}

func TestUnknownAndExpiredAttemptsRetainTheirConservativeCharge(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	controller, _ := New(store, Limits{MonthlyBudgetNanos: 100, MaxConcurrent: 1, LeaseDuration: time.Minute}, func() time.Time { return now })
	lease, err := controller.Reserve(context.Background(), "health", "meal_text_capture", "ark", 60)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Settle(context.Background(), 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reserve(context.Background(), "health", "meal_text_capture", "ark", 50); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("unknown provider result refunded cost: %v", err)
	}

	store = NewMemoryStore()
	controller, _ = New(store, Limits{MonthlyBudgetNanos: 100, MaxConcurrent: 1, LeaseDuration: time.Minute}, func() time.Time { return now })
	if _, err := controller.Reserve(context.Background(), "health", "meal_text_capture", "ark", 60); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	second, err := controller.Reserve(context.Background(), "journal", "journal.organize", "ark", 40)
	if err != nil {
		t.Fatalf("expired concurrency lease did not release its slot: %v", err)
	}
	if err := second.Settle(context.Background(), 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reserve(context.Background(), "journal", "journal.organize", "ark", 1); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expired uncertain call did not retain its charge: %v", err)
	}
}

func TestConcurrentAdmissionNeverExceedsProjectLimit(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	controller, _ := New(store, Limits{MonthlyBudgetNanos: 1_000, MaxConcurrent: 1, LeaseDuration: time.Minute}, func() time.Time { return now })
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := controller.Reserve(context.Background(), "health", "meal_decision", "ark", 1)
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	accepted, rejected := 0, 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrConcurrencyExceeded):
			rejected++
		default:
			t.Fatalf("unexpected result: %v", err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
}

func TestActiveMonthRejectsConflictingReplicaBudget(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	first, _ := New(store, Limits{MonthlyBudgetNanos: 100, MaxConcurrent: 2, LeaseDuration: time.Minute}, func() time.Time { return now })
	second, _ := New(store, Limits{MonthlyBudgetNanos: 200, MaxConcurrent: 2, LeaseDuration: time.Minute}, func() time.Time { return now })
	lease, err := first.Reserve(context.Background(), "health", "meal_decision", "ark", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Settle(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Reserve(context.Background(), "journal", "journal.organize", "ark", 1); !errors.Is(err, ErrConfigurationConflict) {
		t.Fatalf("conflicting replica budget was accepted: %v", err)
	}
}

func TestProviderPricesUseExactIntegerCeilings(t *testing.T) {
	price := TokenPrice{InputNanosPerMillionTokens: 800_000_000, OutputNanosPerMillionTokens: 8_000_000_000}
	cost, err := price.Cost(1, 1)
	if err != nil || cost != 8_800 {
		t.Fatalf("token cost=%d err=%v", cost, err)
	}
	duration, err := (DurationPrice{NanosPerHour: 4_500_000_000}).Cost(15_000)
	if err != nil || duration != 18_750_000 {
		t.Fatalf("duration cost=%d err=%v", duration, err)
	}
}
