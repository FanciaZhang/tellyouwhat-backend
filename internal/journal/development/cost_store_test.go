package development

import (
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCostBudgetSurvivesRestartAndResetsByCalendarMonth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.json")
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	limits := costcontrol.Limits{MonthlyBudgetNanos: 100, MaxConcurrent: 2, LeaseDuration: time.Minute}
	store, err := NewFileCostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := costcontrol.New(store, limits, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.Reserve(context.Background(), "journal-development", "test", "tokens", 70)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.Settle(context.Background(), 30, true); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileCostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	c, _ = costcontrol.New(reopened, limits, func() time.Time { return now })
	if _, err = c.Reserve(context.Background(), "journal-development", "test", "tokens", 71); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatalf("restart reset spending: %v", err)
	}
	now = now.AddDate(0, 1, 0)
	if _, err = c.Reserve(context.Background(), "journal-development", "test", "tokens", 100); err != nil {
		t.Fatalf("new calendar month: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("cost metadata not private")
	}
}
func TestUnsettledCostRemainsChargedAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.json")
	now := time.Now()
	limits := costcontrol.Limits{MonthlyBudgetNanos: 100, MaxConcurrent: 2, LeaseDuration: time.Minute}
	store, _ := NewFileCostStore(path)
	c, _ := costcontrol.New(store, limits, func() time.Time { return now })
	if _, err := c.Reserve(context.Background(), "journal-development", "test", "tokens", 80); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileCostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	c, _ = costcontrol.New(store, limits, func() time.Time { return now })
	if _, err = c.Reserve(context.Background(), "journal-development", "test", "tokens", 21); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatal("unsettled spending disappeared")
	}
	if err = os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = NewFileCostStore(path); err == nil {
		t.Fatal("corrupt state silently reset")
	}
}

func TestRestartReleasesDeadConcurrencyWithoutRefundingUncertainCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.json")
	now := time.Now()
	limits := costcontrol.Limits{MonthlyBudgetNanos: 100, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}
	store, err := NewFileCostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	controller, _ := costcontrol.New(store, limits, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if _, err = controller.Reserve(context.Background(), "journal-development", "voice", "tokens", 30); err != nil {
			t.Fatal(err)
		}
	}
	// Restart while ASR and the editor both have an outstanding provider call.
	store, err = NewFileCostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	controller, _ = costcontrol.New(store, limits, func() time.Time { return now })
	lease, err := controller.Reserve(context.Background(), "journal-development", "voice", "tokens", 40)
	if err != nil {
		t.Fatalf("dead process still occupies provider slots: %v", err)
	}
	if err = lease.Settle(context.Background(), 0, true); err != nil {
		t.Fatal(err)
	}
	// The two interrupted requests still conservatively cost 60, across restarts.
	store, err = NewFileCostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	controller, _ = costcontrol.New(store, limits, func() time.Time { return now })
	if _, err = controller.Reserve(context.Background(), "journal-development", "voice", "tokens", 41); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatalf("unknown spending was refunded: %v", err)
	}
}
