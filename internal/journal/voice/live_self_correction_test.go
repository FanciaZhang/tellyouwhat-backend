package voice

import (
	"context"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"
)

// Synthetic text checks complement the App's real ASR/audio acceptance.
func TestLiveSelfCorrectionAcrossWritingStyles(t *testing.T) {
	if os.Getenv("JOURNAL_SELF_CORRECTION_LIVE_CHECK") != "1" {
		t.Skip("explicit development model acceptance")
	}
	model := developmentEditRewriter{base: os.Getenv("JOURNAL_EDIT_INTENT_SERVICE_URL")}
	for _, style := range []WritingStyle{StyleNatural, StyleLively, StyleDaybook, StyleEssay, StyleDocumentary} {
		for _, marker := range []string{"啊呸，不对", "啊不对"} {
			t.Run(string(style)+"/"+marker, func(t *testing.T) {
				s := polishFixture()
				s.WritingStyle = style
				source := "今年我已经25岁了。" + marker + "，已经是30岁了。"
				s.Blocks[0].Text = source
				s.Polish.Targets[0].Text = source
				s.Polish.Targets[0].SourceText = source
				ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
				defer cancel()
				result, err := model.Rewrite(ctx, s, 0)
				if err != nil {
					t.Fatal(err)
				}
				assertCorrectedAge(t, result)
			})
		}
	}
	t.Run("correction_of_retained_fact", func(t *testing.T) {
		s := polishFixture()
		s.WritingStyle = StyleNatural
		s.Polish.Targets[0].RetainedText = "今年我已经25岁了。"
		s.Polish.Targets[0].SourceText = "啊不对，已经是30岁了。"
		s.Polish.Targets[0].Text = s.Polish.Targets[0].RetainedText + s.Polish.Targets[0].SourceText
		s.Blocks[0].Text = s.Polish.Targets[0].Text
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()
		result, err := model.Rewrite(ctx, s, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertCorrectedAge(t, result)
	})
	t.Run("two_writable_paragraphs", func(t *testing.T) {
		s := polishFixture()
		s.WritingStyle = StyleNatural
		s.Polish.Targets[0].Text = "今年我已经25岁了。"
		s.Polish.Targets[0].RetainedText = s.Polish.Targets[0].Text
		s.Polish.Targets[0].SourceText = s.Polish.Targets[0].Text
		second := PolishTarget{ID: uuid.NewString(), Style: "body", Text: "啊不对，已经是30岁了。", SourceText: "啊不对，已经是30岁了。", SourceIDs: []string{uuid.NewString()}}
		s.Polish.Targets = append(s.Polish.Targets, second)
		s.Blocks = []Block{{ID: s.Polish.Targets[0].ID, Text: s.Polish.Targets[0].Text, Style: "body"}, {ID: second.ID, Text: second.Text, Style: "body"}}
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()
		result, err := model.Rewrite(ctx, s, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertCorrectedAge(t, result)
	})
	t.Run("explicitly_narrated_event_is_preserved", func(t *testing.T) {
		s := polishFixture()
		s.WritingStyle = StyleEssay
		source := "今天跟朋友聊天，我说今年25岁了，又说啊呸不对，今年30岁了。大家都笑了。"
		s.Blocks[0].Text = source
		s.Polish.Targets[0].Text = source
		s.Polish.Targets[0].SourceText = source
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()
		result, err := model.Rewrite(ctx, s, 0)
		if err != nil {
			t.Fatal(err)
		}
		if result.Polish == nil {
			t.Fatal("missing narrative")
		}
		var body string
		for _, p := range result.Polish.Paragraphs {
			body += p.Text
		}
		t.Log(body)
		if !strings.Contains(body, "朋友") || !strings.Contains(body, "笑") || !strings.Contains(body, "30") {
			t.Fatal("lost explicitly narrated event")
		}
		for _, invented := range []string{"好几次", "反复", "自己都觉得好笑"} {
			if strings.Contains(body, invented) {
				t.Fatal("invented correction event")
			}
		}
	})
}
func assertCorrectedAge(t *testing.T, result RewriteResult) {
	t.Helper()
	if result.Polish == nil {
		t.Fatal("missing narrative")
	}
	var body string
	for _, p := range result.Polish.Paragraphs {
		body += p.Text
	}
	t.Log(body)
	if !strings.Contains(body, "30岁") {
		t.Fatal("missing confirmed age")
	}
	for _, old := range []string{"25", "二十五", "啊呸", "啊不对", "顺口", "纠正", "口误", "说错", "好几次", "觉得好笑"} {
		if strings.Contains(body, old) {
			t.Fatalf("retained/invented correction %s", old)
		}
	}
}
