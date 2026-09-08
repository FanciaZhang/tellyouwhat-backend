package voice

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"strings"
	"testing"
)

func TestDynamicStyleAndEditorialRulesUseFrozenConfiguration(t *testing.T) {
	policy := promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]
	policy.Journal.Styles = append(policy.Journal.Styles, promptconfig.Style{ID: "custom-style", Name: "新风格", Prompt: "保留短句", Enabled: true})
	policy.Journal.Voice.RemoveRepetition = false
	policy.Journal.Voice.Parameters.MaxOutputTokens = 4096
	revision := promptconfig.Revision{ID: "frozen", Scope: "journal", Policy: policy}
	ctx := promptconfig.WithRevision(context.Background(), revision)
	snapshot := Snapshot{WritingStyle: "custom-style", Blocks: []Block{{uuid.NewString(), ""}}, Transcript: "合成口述"}
	prepared, err := PrepareRewrite(ctx, snapshot, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(prepared.Body, &body)
	instructions := body["instructions"].(string)
	if !strings.Contains(instructions, "保留短句") || strings.Contains(instructions, "删除口头重复") || body["max_output_tokens"] != float64(4096) {
		t.Fatal("configuration not applied")
	}
	revision.Policy.Journal.Styles[len(revision.Policy.Journal.Styles)-1].Prompt = "changed after freeze"
	same, err := PrepareRewrite(ctx, snapshot, 1, "")
	if err != nil || string(same.Body) != string(prepared.Body) {
		t.Fatal("frozen configuration mutated")
	}
	// Disabled choices remain valid for existing manuscripts, but unknown IDs fail.
	policy.Journal.Styles[len(policy.Journal.Styles)-1].Enabled = false
	ctx = promptconfig.WithRevision(context.Background(), promptconfig.Revision{ID: "disabled", Scope: "journal", Policy: policy})
	if _, err := PrepareRewrite(ctx, snapshot, 1, ""); err != nil {
		t.Fatal("existing disabled style was replaced", err)
	}
	snapshot.WritingStyle = "missing"
	if _, err := PrepareRewrite(ctx, snapshot, 1, ""); err == nil {
		t.Fatal("unknown style accepted")
	}
}
