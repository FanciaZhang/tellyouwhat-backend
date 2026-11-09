package voice

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

// Replay only explicitly supplied synthetic App evidence. No private recording
// or provider credential is stored in the resulting diagnostic artifact.
func TestConfiguredIdentityReplayFromSyntheticApp(t *testing.T) {
	path := os.Getenv("JOURNAL_IDENTITY_REPLAY_CONFIG")
	if path == "" {
		t.Skip("explicit configured-provider synthetic identity replay only")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private configuration")
	}
	var config map[string]string
	if json.Unmarshal(data, &config) != nil || config["JOURNAL_ARK_API_KEY"] == "" || config["InputPath"] == "" || config["OutputPath"] == "" {
		t.Fatal("invalid private replay configuration")
	}
	data, err = os.ReadFile(config["InputPath"])
	if err != nil {
		t.Fatal("read synthetic request")
	}
	var request IdentityRequest
	if json.Unmarshal(data, &request) != nil || request.Validate() != nil {
		t.Fatal("invalid captured identity request")
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 200 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	model := NewBudgetedRewriter(ArkRewriter{BaseURL: config["JOURNAL_ARK_BASE_URL"], APIKey: config["JOURNAL_ARK_API_KEY"], Model: config["JOURNAL_VOICE_MODEL"]}, budget,
		"journal-development", costcontrol.TokenPrice{InputNanosPerMillionTokens: 2_000_000_000, OutputNanosPerMillionTokens: 12_000_000_000})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := model.Rewrite(ctx, Snapshot{Identity: &request, DictationMode: true, WritingStyle: "natural", Blocks: []Block{{ID: uuid.NewString(), Text: "合成语音原话", Style: "body"}}}, 1)
	record := map[string]any{"synthetic": true, "appAcceptance": false, "request": request, "response": result.Identity, "raw": result.OutputText,
		"model": result.Model, "stage": result.Diagnostics.Stage, "inputTokens": result.InputTokens, "outputTokens": result.OutputTokens}
	if err != nil {
		record["error"] = err.Error()
	}
	data, e := json.MarshalIndent(record, "", "  ")
	if e == nil {
		e = os.WriteFile(config["OutputPath"], data, 0600)
	}
	if e != nil {
		t.Fatal("preserve synthetic replay result")
	}
	if err != nil {
		t.Fatal("configured identity replay failed; provider output preserved", err)
	}
	if result.Identity == nil {
		t.Fatal("missing identity result")
	}
	if unknown := config["ExpectedUnknownSourceIDs"]; unknown != "" {
		for _, assignment := range result.Identity.Assignments {
			for _, id := range strings.Split(unknown, ",") {
				if slices.Contains(assignment.SourceIDs, id) {
					t.Error("shared acoustic key/topic cannot resolve the ambiguous later speaker", id)
				}
			}
		}
	}
	if name := config["ExpectedName"]; name != "" {
		claimed := map[string]bool{}
		for _, assignment := range result.Identity.Assignments {
			if assignment.Name != name {
				continue
			}
			if assignment.Scope != "sources" || assignment.PersonID != "" {
				t.Error("a different person must not rename or reuse the prior voice identity")
			}
			for _, id := range assignment.SourceIDs {
				claimed[id] = true
			}
		}
		for _, id := range strings.Split(config["ExpectedSourceIDs"], ",") {
			if !claimed[id] {
				t.Error("self-introduction and grounded later experience must retain the third person", id)
			}
		}
		for id := range claimed {
			if slices.Contains(strings.Split(config["ExcludedSourceIDs"], ","), id) {
				t.Error("third-person inference took another participant's source", id)
			}
		}
	}
}
