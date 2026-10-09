package voice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// Explicit diagnostic for synthetic App-generated snapshots only. Provider
// credentials stay in the server process environment and are never printed.
func TestLiveSyntheticCommandSnapshot(t *testing.T) {
	if os.Getenv("JOURNAL_SYNTHETIC_COMMAND_CHECK") != "1" {
		t.Skip("explicit synthetic command diagnostic")
	}
	raw, err := os.ReadFile(os.Getenv("JOURNAL_SYNTHETIC_COMMAND_SNAPSHOT"))
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Polish != nil || len(s.PendingUtterances) == 0 {
		t.Fatal("expected App structural-command snapshot")
	}
	if err = s.Validate(); err != nil {
		t.Fatal("App snapshot invalid", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := (ArkRewriter{BaseURL: os.Getenv("JOURNAL_ARK_BASE_URL"), APIKey: os.Getenv("JOURNAL_ARK_API_KEY"), Model: os.Getenv("JOURNAL_VOICE_MODEL")}).Rewrite(ctx, s, 0)
	t.Logf("stage=%s synthetic_output=%s", result.Diagnostics.Stage, result.OutputText)
	if err != nil {
		t.Fatal(err, errors.Unwrap(err))
	}
}
