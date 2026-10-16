package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/development"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

// Maintenance runs this only in an isolated container with copied recovery files.
func verifyRecovery() error {
	if os.Getenv("JOURNAL_PROMPT_FILE") != "/restore/config/prompt.json" || os.Getenv("JOURNAL_DEVELOPMENT_STATE_DIR") != "/restore/state" ||
		os.Getenv("JOURNAL_DEVELOPMENT_TOKEN") != "" || os.Getenv("DATABASE_DSN") != "" || os.Getenv("REDIS_URL") != "" || os.Getenv("JOURNAL_PROMPT_DATABASE_DSN") != "" {
		return errors.New("recovery verification requires an isolated copy without connection credentials")
	}
	budget, err := strconv.ParseInt(os.Getenv("JOURNAL_DEVELOPMENT_MONTHLY_BUDGET_CNY"), 10, 64)
	if err != nil || budget < 1 || budget > 1000 {
		return errors.New("invalid recovery budget")
	}
	cache := promptconfig.NewJournalCache(journalPromptFile{path: "/restore/config/prompt.json"})
	if err := cache.Refresh(context.Background(), time.Now()); err != nil {
		return errors.New("invalid recovered Journal Prompt")
	}
	if _, err := development.NewFileCostStore("/restore/state/cost-events.json", budget*costcontrol.NanosPerCNY, time.Now()); err != nil {
		return errors.New("invalid recovered Journal cost ledger")
	}
	revision, _ := cache.Current("journal")
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"passed": true, "prompt_revision": revision.ID, "cost_store_replayed": true})
}
