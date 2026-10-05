package voice

import (
	"context"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in real-model acceptance. Only invented trip preparations are sent.
func TestLiveNumberedSpeech(t *testing.T) {
	if os.Getenv("JOURNAL_NUMBERED_SPEECH_LIVE_CHECK") != "1" {
		t.Skip("explicit synthetic development acceptance")
	}
	model := developmentEditRewriter{base: os.Getenv("JOURNAL_EDIT_INTENT_SERVICE_URL")}
	for _, tc := range []struct {
		name, text string
		context    []Block
		count      int
	}{
		{"three_points", "所以出行前的准备其实分三个点：第一，我要提前买车票。第二，我要收拾行李。第三，天气可能会变冷，但我还没有带外套，得先拿一件。最后，希望这次出行顺利。", nil, 3},
		{"continuation", "第二点，我要收拾行李。第三点，天气可能会变冷，得先拿一件外套。最后，希望这次出行顺利。", []Block{{ID: uuid.NewString(), Text: "我要提前买车票。", Style: "orderedListItem"}}, 2},
		{"continued_draft", "第二点，我要收拾行李。第三点，天气可能会变冷，得先拿一件外套。最后，希望这次出行顺利。", nil, 3},
		{"ordinary_narrative", "第一天我坐火车去了杭州。第二天在湖边散步，第三天回家。", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := polishFixture()
			s.WritingStyle = StyleNatural
			s.Blocks[0].Text = tc.text
			s.Polish.Targets[0].Text = tc.text
			s.Polish.Targets[0].SourceText = tc.text
			s.Polish.Context = tc.context
			if tc.name == "continued_draft" {
				s.Polish.Targets[0].Style = "orderedListItem"
				s.Polish.Targets[0].RetainedText = "我要提前买车票。"
				s.Polish.Targets[0].Text = "我要提前买车票。" + tc.text
				s.Blocks[0].Text = s.Polish.Targets[0].Text
				s.Blocks[0].Style = "orderedListItem"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			defer cancel()
			result, err := model.Rewrite(ctx, s, 0)
			if err != nil {
				t.Fatal(err)
			}
			if result.Polish == nil {
				t.Fatal("missing narrative")
			}
			count := 0
			var text strings.Builder
			for _, p := range result.Polish.Paragraphs {
				if p.Style == "orderedListItem" {
					count++
					if len(spokenListMarker.FindAllStringIndex(p.Text, -1)) >= 2 {
						t.Fatal("collapsed list")
					}
				}
				text.WriteString(p.Text)
				t.Logf("%s: %s", p.Style, p.Text)
			}
			if count != tc.count {
				t.Fatalf("want %d independent points, got %d", tc.count, count)
			}
			if tc.count > 0 {
				last := result.Polish.Paragraphs[len(result.Polish.Paragraphs)-1]
				if last.Style != "body" || !strings.Contains(last.Text, "顺利") {
					t.Fatal("conclusion did not leave list")
				}
				if !strings.Contains(text.String(), "外套") || !strings.Contains(text.String(), "行李") {
					t.Fatal("details lost")
				}
			}
		})
	}
}
