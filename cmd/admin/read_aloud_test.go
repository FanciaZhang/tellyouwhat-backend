package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

func TestReadAloudServicesAreExplicitAndBudgeted(t *testing.T) {
	t.Setenv("JOURNAL_TTS_INVENTORY_CREDENTIALS_FILE", "")
	t.Setenv("JOURNAL_TTS_API_KEY_FILE", "")
	t.Setenv("JOURNAL_TTS_PROBE_DAILY_CHARACTERS", "")
	limits := costcontrol.Limits{MonthlyBudgetNanos: costcontrol.NanosPerCNY * 10, MaxConcurrent: 2, LeaseDuration: time.Minute}
	d, p, err := readAloudServices(nil, limits)
	if err != nil || d != nil || p != nil {
		t.Fatal("unconfigured services enabled", err)
	}
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, []byte("fixture-key"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JOURNAL_TTS_API_KEY_FILE", file)
	d, p, err = readAloudServices(nil, limits)
	if err != nil || d != nil || p == nil || p.Provider == nil || p.Quotas == nil || p.DailyCharacters != 33333 {
		t.Fatal("missing budgeted probe wiring", err)
	}
	t.Setenv("JOURNAL_TTS_PROBE_DAILY_CHARACTERS", "-1")
	if _, _, err := readAloudServices(nil, limits); err == nil {
		t.Fatal("invalid allowance accepted")
	}
	t.Setenv("JOURNAL_TTS_PROBE_DAILY_CHARACTERS", "1000")
	if _, _, err := readAloudServices(nil, costcontrol.Limits{}); err == nil {
		t.Fatal("provider enabled without project budget")
	}
}
