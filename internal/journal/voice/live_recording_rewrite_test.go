package voice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicitly authorized source stays outside the repository and test logs.
// Reuses existing transcription; does not submit audio or consume voice minutes.
func TestLiveRecordingRewriteComparison(t *testing.T) {
	if os.Getenv("JOURNAL_LIVE_RECORDING_REWRITE") != "1" {
		t.Skip("explicit live rewrite opt-in required")
	}
	raw, err := os.ReadFile(os.Getenv("JOURNAL_LIVE_RECORDING_REWRITE_INPUT"))
	if err != nil {
		t.Fatal(err)
	}
	var input Snapshot
	if json.Unmarshal(raw, &input) != nil || input.Validate() != nil || input.RecordingContext == nil {
		t.Fatal("invalid private benchmark input")
	}
	model := ArkRewriter{BaseURL: os.Getenv("JOURNAL_ARK_BASE_URL"), APIKey: os.Getenv("JOURNAL_ARK_API_KEY"), Model: os.Getenv("JOURNAL_VOICE_MODEL")}
	turns, err := RenderRecordingDialogue(*input.RecordingContext)
	if err != nil {
		t.Fatal(err)
	}
	dialogue, _ := json.MarshalIndent(turns, "", "  ")
	if err := os.WriteFile(filepath.Join(os.Getenv("JOURNAL_LIVE_RECORDING_REWRITE_OUTPUT"), "dialogue.json"), dialogue, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"narrative"} {
		snapshot := input
		if mode == "baseline" {
			snapshot.RecordingContext = nil
		} else {
			copy := *input.RecordingContext
			copy.Mode = mode
			snapshot.RecordingContext = &copy
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		start := time.Now()
		result, err := model.Rewrite(ctx, snapshot, 1)
		cancel()
		if err != nil {
			t.Fatal(mode, err)
		}
		encoded, _ := json.MarshalIndent(map[string]any{"mode": mode, "elapsedSeconds": time.Since(start).Seconds(), "result": result}, "", "  ")
		if err := os.WriteFile(filepath.Join(os.Getenv("JOURNAL_LIVE_RECORDING_REWRITE_OUTPUT"), mode+".json"), encoded, 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s elapsed=%s inputTokens=%d outputTokens=%d", mode, time.Since(start), result.InputTokens, result.OutputTokens)
	}
}

// Private offline acceptance: no provider call and no source text in test logs.
func TestPrivateRecordingDraftReview(t *testing.T) {
	if os.Getenv("JOURNAL_PRIVATE_RECORDING_REVIEW") != "1" {
		t.Skip("explicit private fixture opt-in required")
	}
	raw, err := os.ReadFile(os.Getenv("JOURNAL_LIVE_RECORDING_REWRITE_INPUT"))
	if err != nil {
		t.Fatal(err)
	}
	var input Snapshot
	if json.Unmarshal(raw, &input) != nil || input.Validate() != nil || input.RecordingContext == nil {
		t.Fatal("invalid private input")
	}
	raw, err = os.ReadFile(os.Getenv("JOURNAL_PRIVATE_RECORDING_DRAFT"))
	if err != nil {
		t.Fatal(err)
	}
	var draft struct{ Result RewriteResult }
	if json.Unmarshal(raw, &draft) != nil {
		t.Fatal("invalid draft")
	}
	text := ""
	for _, patch := range draft.Result.Revision.Patches {
		text += patch.Text + "\n"
	}
	issues := ReviewRecordingDraft(input.RecordingContext.Analysis, text)
	turns, err := RenderRecordingDialogue(*input.RecordingContext)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	sourceText := ""
	for _, turn := range turns {
		ids = append(ids, turn.UtteranceIDs...)
		sourceText += turn.Text
	}
	expectedText := ""
	for i, u := range input.RecordingContext.Analysis.Utterances {
		expectedText += u.Text
		if i >= len(ids) || ids[i] != u.ID {
			t.Fatal("dialogue lost source ordering")
		}
	}
	if len(ids) != len(input.RecordingContext.Analysis.Utterances) || sourceText != expectedText {
		t.Fatal("dialogue lost source content")
	}
	result := map[string]any{"reviewIssues": issues, "speakerEvidence": input.RecordingContext.Analysis.SpeakerEvidence(), "dialogueSourcePreserved": true, "sourceUtterances": len(ids), "dialogueTurns": len(turns)}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(os.Getenv("JOURNAL_PRIVATE_RECORDING_REVIEW_OUTPUT"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("reviewIssues=%d sourceUtterances=%d dialogueTurns=%d; private evidence saved", len(issues), len(ids), len(turns))
}
