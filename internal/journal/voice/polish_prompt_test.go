package voice

import (
	"bytes"
	"context"
	"testing"

	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func TestPolishPublishedPromptReachesFinalizationOnly(t *testing.T) {
	snapshot := comparisonSnapshot([]string{"今天下班后，我去河边散步。"}, false)
	policy := promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]
	revision := promptconfig.Revision{ID: "test", Scope: "journal", Policy: policy}
	baseline, err := PrepareRewrite(promptconfig.WithRevision(context.Background(), revision), snapshot, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	policy.Journal.Voice.PolishPrompt = "调试成稿：按事件分段，保留具体地点。"
	revision.Policy = policy
	prepared, err := PrepareRewrite(promptconfig.WithRevision(context.Background(), revision), snapshot, 1, "")
	if err != nil || !bytes.Contains(prepared.Body, []byte(policy.Journal.Voice.PolishPrompt)) || bytes.Contains(baseline.Body, []byte(policy.Journal.Voice.PolishPrompt)) {
		t.Fatal("published finalization prompt missing", err)
	}
	snapshot.Polish.Lightweight = true
	lightweight, err := PrepareRewrite(promptconfig.WithRevision(context.Background(), revision), snapshot, 1, "")
	if err != nil || bytes.Contains(lightweight.Body, []byte(policy.Journal.Voice.PolishPrompt)) {
		t.Fatal("finalization rules leaked into lightweight correction", err)
	}
}
