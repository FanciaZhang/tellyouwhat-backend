package voice

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Operator-only acceptance against the configured model. All text is synthetic;
// do not enable this in ordinary tests or replace it with a user's journal.
func TestLiveManualEditIntent(t *testing.T) {
	if os.Getenv("JOURNAL_EDIT_INTENT_LIVE_CHECK") != "1" {
		t.Skip("live editorial acceptance requires explicit activation")
	}
	base := os.Getenv("JOURNAL_ARK_BASE_URL")
	if base == "" {
		base = "https://ark.cn-beijing.volces.com/api/v3"
	}
	model := os.Getenv("JOURNAL_VOICE_MODEL")
	if model == "" {
		model = os.Getenv("JOURNAL_VOICE_MODEL_ID")
	}
	var rewriter Rewriter = ArkRewriter{BaseURL: base, APIKey: os.Getenv("JOURNAL_ARK_API_KEY"), Model: model}
	if service := os.Getenv("JOURNAL_EDIT_INTENT_SERVICE_URL"); service != "" {
		rewriter = developmentEditRewriter{base: service}
	}
	var details strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&details, "路过第%d家店时，我停下来看看橱窗，记住了当时的光线和街边的树。", i)
	}
	for _, tc := range []struct{ name, current, transcript, expected, before, after, contextBefore, contextAfter string }{
		{"punctuation_in_long_paragraph", "上午九点，我和小明去了公园。" + details.String(),
			"上午九点我和小明去了公园。补充纠正一下，前面说错了人名，陪我去公园的是小林，不是小明。",
			"上午九点，我和小林去了公园。" + details.String(), ",", "，", "上午九点", "我和小明去了公园。"},
		{"explicit_correction_of_manual_words", "门票花了十二元。我们在湖边坐了一会儿。",
			"我刚才又核对了票据，前面手动改成十二元也不对，实际门票是十五元。",
			"门票花了十五元。我们在湖边坐了一会儿。", "", "二", "门票花了十", "元。"},
		{"old_transcript_does_not_undo_current_edit", "上午九点，我和小林去了公园。",
			"上午九点我和小明去了公园。",
			"上午九点，我和小林去了公园。", "明", "林", "上午九点，我和小", "去了公园。"},
		{"old_explicit_correction_does_not_undo_later_manual_edit", "门票花了十五元。",
			"门票是十二元。不对，门票应该是十元。",
			"门票花了十五元。", "", "五", "门票花了十", "元。"},
		{"pending_earlier_correction_does_not_undo_manual_edit", "门票花了十五元。",
			"门票是十二元。不对，门票应该是十元。",
			"门票花了十五元。", "", "五", "门票花了十", "元。"},
		{"explicit_uncertainty_does_not_invent_certainty", "周六我和小林去了公园。",
			"同行的人可能是小明，也可能不是，我记不清了。",
			"周六我和小林去了公园。", "明", "林", "周六我和小", "去了公园。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.NewString()
			snapshot := Snapshot{Revision: 2, Blocks: []Block{{id, tc.current}}, Transcript: tc.transcript, EditedBlockIDs: []string{id}, ManualEdits: []ManualEdit{{BlockID: id, Before: tc.before, After: tc.after, ContextBefore: tc.contextBefore, ContextAfter: tc.contextAfter}}}
			if strings.HasPrefix(tc.name, "pending_") {
				snapshot.ManualEdits[0].PendingEarlierSpeech = true
			}
			if strings.HasPrefix(tc.name, "old_") {
				snapshot.ManualEdits[0].TranscriptOffset = len([]rune(tc.transcript))
			}
			if tc.name == "punctuation_in_long_paragraph" {
				snapshot.ManualEdits[0].TranscriptOffset = len([]rune("上午九点我和小明去了公园。"))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			result, err := rewriter.Rewrite(ctx, snapshot, 3)
			if err != nil {
				t.Fatal(err)
			}
			actual := tc.current
			for _, p := range result.Revision.Patches {
				if p.ID != id || p.AfterID != "" {
					t.Fatal("correction appended a new paragraph instead of editing in place")
				}
				actual = p.Text
			}
			if tc.name == "explicit_uncertainty_does_not_invent_certainty" {
				// This is a semantic property check, not a guarantee that the model
				// always preserves the old sentence or asks a question. Expressing
				// the user's own uncertainty in the body is also a valid outcome.
				uncertain := strings.Contains(actual, "可能") || strings.Contains(actual, "不确定") || strings.Contains(actual, "记不清")
				if actual == tc.expected {
					if len(result.Revision.Questions) == 0 {
						t.Fatal("uncertainty was silently discarded")
					}
				} else if !uncertain || !strings.Contains(actual, "周六") || !strings.Contains(actual, "公园") || strings.Contains(actual, "我和小明去了公园") {
					t.Fatalf("uncertainty must not invent certainty or lose the event: %q", actual)
				}
				t.Logf("verified expressed uncertainty; synthetic output: %q", actual)
			} else if actual != tc.expected {
				t.Fatalf("editorial outcome mismatch: expected %d runes, received %d; synthetic output: %q", len([]rune(tc.expected)), len([]rune(actual)), string([]rune(actual)[:min(200, len([]rune(actual)))]))
			} else {
				t.Log("verified exact in-place edit with unrelated content preserved")
			}

		})
	}
}
