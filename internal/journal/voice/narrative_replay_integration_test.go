package voice

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
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
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 200 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	model := NewBudgetedRewriter(ArkRewriter{BaseURL: config.BaseURL, APIKey: config.APIKey, Model: config.Model}, budget, "journal-development", costcontrol.TokenPrice{InputNanosPerMillionTokens: 2_000_000_000, OutputNanosPerMillionTokens: 12_000_000_000})
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
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
