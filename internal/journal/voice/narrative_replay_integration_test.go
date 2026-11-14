package voice

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

// Only an explicitly supplied synthetic App request can enable this check.
// The original snapshot, raw provider output and applied revision are retained.
func TestConfiguredNarrativeReplayFromSyntheticApp(t *testing.T) {
	path := os.Getenv("JOURNAL_NARRATIVE_REPLAY_CONFIG")
	if path == "" {
		t.Skip("explicit actual-model synthetic App narrative replay only")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private configuration")
	}
	var config struct {
		Revision                            *promptconfig.Revision
		MaxCostNanos                        int64
		BaseURL                             string `json:"JOURNAL_ARK_BASE_URL"`
		APIKey                              string `json:"JOURNAL_ARK_API_KEY"`
		Model                               string `json:"JOURNAL_VOICE_MODEL"`
		InputPath, OutputPath               string
		RequiredPatterns, ForbiddenPatterns []string
	}
	if json.Unmarshal(data, &config) != nil || config.APIKey == "" || config.Model == "" || config.InputPath == "" || config.OutputPath == "" {
		t.Fatal("invalid private configuration")
	}
	data, err = os.ReadFile(config.InputPath)
	if err != nil {
		t.Fatal("read synthetic snapshot")
	}
	var snapshot Snapshot
	if json.Unmarshal(data, &snapshot) != nil || snapshot.Validate() != nil || snapshot.Polish == nil {
		t.Fatal("invalid captured synthetic narrative snapshot")
	}
	if config.MaxCostNanos == 0 {
		config.MaxCostNanos = costcontrol.NanosPerCNY
	}
	if config.MaxCostNanos < 0 || config.MaxCostNanos > costcontrol.NanosPerCNY {
		t.Fatal("bounded replay budget required")
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: config.MaxCostNanos, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	model := NewBudgetedRewriter(ArkRewriter{BaseURL: config.BaseURL, APIKey: config.APIKey, Model: config.Model}, budget, "journal-development", costcontrol.TokenPrice{InputNanosPerMillionTokens: 9_600_000_000, OutputNanosPerMillionTokens: 48_000_000_000})
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	if config.Revision != nil {
		ctx = promptconfig.WithRevision(ctx, *config.Revision)
	}
	result, callErr := model.Rewrite(ctx, snapshot, 1)
	text := ""
	if result.Polish != nil {
		for _, p := range result.Polish.Paragraphs {
			text += p.Text + "\n"
		}
	}
	record := map[string]any{"synthetic": true, "appAcceptance": false, "snapshot": snapshot, "raw": result.OutputText, "revision": result.Polish, "text": text, "model": result.Model, "stage": result.Diagnostics.Stage, "diagnostics": result.Diagnostics, "inputTokens": result.InputTokens, "outputTokens": result.OutputTokens}
	if callErr != nil {
		record["error"] = callErr.Error()
	}
	data, err = json.MarshalIndent(record, "", "  ")
	if err != nil || os.WriteFile(config.OutputPath, data, 0600) != nil {
		t.Fatal("preserve actual replay")
	}
	if callErr != nil || result.Polish == nil {
		t.Fatal("real narrative replay failed; original output preserved", callErr)
	}
	for _, pattern := range config.RequiredPatterns {
		if !regexp.MustCompile(pattern).MatchString(text) {
			t.Error("missing required source relationship", pattern)
		}
	}
	for _, pattern := range config.ForbiddenPatterns {
		if regexp.MustCompile(pattern).MatchString(text) {
			t.Error("invented relationship or pronoun", pattern)
		}
	}
}

// Re-run validation against preserved real synthetic output without buying a
// second response merely to diagnose a decoder or normalization change.
func TestCapturedNarrativeResponseDecodesWithoutNetwork(t *testing.T) {
	path := os.Getenv("JOURNAL_CAPTURED_NARRATIVE_RESPONSE")
	if path == "" {
		t.Skip("explicit captured synthetic response only")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read captured synthetic response")
	}
	var record struct {
		Snapshot Snapshot
		Raw      string
	}
	if json.Unmarshal(data, &record) != nil || record.Snapshot.Validate() != nil || record.Snapshot.Polish == nil {
		t.Fatal("invalid captured App snapshot")
	}
	result, err := decodePolish(record.Raw, *record.Snapshot.Polish)
	if err != nil || len(result.Clarifications) == 0 {
		t.Fatal("captured choices still fail validation", err)
	}
}
