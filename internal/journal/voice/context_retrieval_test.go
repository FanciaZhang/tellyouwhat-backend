package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func retrievalFixture(speech string) Snapshot {
	blocks := []Block{}
	for i := 0; i < 30; i++ {
		blocks = append(blocks, Block{ID: uuid.NewString(), Text: "普通的一天。", Style: "body"})
	}
	source := uuid.NewString()
	return Snapshot{Blocks: blocks, ActiveBlockIDs: []string{blocks[29].ID}, KnownSourceIDs: []string{source},
		PendingUtterances: []SourceUtterance{{ID: source, Text: speech}}}
}

func modelContext(t *testing.T, s Snapshot) rewriteModelDocument {
	t.Helper()
	raw, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var result rewriteModelDocument
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHistoricalContextFindsSpokenQuoteTopicAndOrdinal(t *testing.T) {
	for _, speech := range []string{"把“这次预算需要重新核对”标黄", "把讲费用的那段移到最后", "把第三段放到前面", "把第3个段落标黄"} {
		t.Run(speech, func(t *testing.T) {
			s := retrievalFixture(speech)
			s.Blocks[2].Text = "费用：这次预算需要重新核对。"
			result := modelContext(t, s)
			if !slices.ContainsFunc(result.ContextBlocks, func(b Block) bool { return b.ID == s.Blocks[2].ID }) {
				t.Fatal("historical target omitted")
			}
			if !slices.ContainsFunc(result.ContextBlocks, func(b Block) bool { return b.ID == s.Blocks[29].ID }) {
				t.Fatal("active paragraph omitted")
			}
			if slices.Contains(result.ReplaceableBlockIDs, s.Blocks[2].ID) {
				t.Fatal("retrieval granted rewrite permission")
			}
			if result.DocumentBlockCount != 30 || !slices.ContainsFunc(result.ContextTargets, func(c ContextTarget) bool { return c.BlockID == s.Blocks[2].ID && c.Ordinal == 3 }) {
				t.Fatal("missing stable ordinal")
			}
		})
	}
}

func TestHistoricalContextKeepsAmbiguityOutsideWindow(t *testing.T) {
	s := retrievalFixture("把“这段值得记住”加粗")
	for i := 0; i < 20; i++ {
		s.Blocks[i].Text = "这段值得记住。"
	}
	result := modelContext(t, s)
	if len(result.ContextBlocks) > 6 || len(result.ContextTargets) == 0 || len(result.ContextTargets) >= 20 {
		t.Fatal("unbounded retrieval")
	}
	for _, target := range result.ContextTargets {
		if target.MatchCount != 20 {
			t.Fatal("hidden ambiguity")
		}
	}
}

func TestHistoricalMoveRetainsDestinationAlongsideAmbiguousSource(t *testing.T) {
	s := retrievalFixture("把“费用说明”移到“最后的决定”后面")
	for i := 0; i < 20; i++ {
		s.Blocks[i].Text = "费用说明：等待补充。"
	}
	s.Blocks[24].Text = "最后的决定：先做小范围验证。"
	result := modelContext(t, s)
	if !slices.ContainsFunc(result.ContextTargets, func(c ContextTarget) bool { return c.BlockID == s.Blocks[24].ID && c.MatchCount == 1 }) {
		t.Fatal("ambiguous source crowded out distinct destination")
	}
}

func TestHistoricalLongQuoteKeepsNewSpeechAndRuneBudget(t *testing.T) {
	s := retrievalFixture("把“非常重要的费用说明”标黄")
	s.Blocks[0].Text = "非常重要的费用说明" + strings.Repeat("旧内容", 3000)
	s.Blocks[29].Text = "新内容必须继续可见" + strings.Repeat("新的话", 2000)
	result := modelContext(t, s)
	total, found := 0, false
	for _, block := range result.ContextBlocks {
		total += utf8.RuneCountInString(block.Text)
		if block.ID == s.Blocks[0].ID {
			found = strings.Contains(block.Text, "非常重要的费用说明")
		}
		if block.ID == s.Blocks[29].ID && utf8.RuneCountInString(block.Text) < 100 {
			t.Fatal("active context starved")
		}
	}
	if !found || total > MaxRewriteContextCharacters {
		t.Fatal("lost quote or exceeded budget")
	}
}

func TestSpokenOrdinalRejectsAmbiguousOrMixedNumerals(t *testing.T) {
	for text, want := range map[string]int{"十二": 12, "二十三": 23, "一百零三": 103, "两百": 200, "一百二十三": 123, "12": 12, "一二": 0, "1十": 0, "一百二": 0, "一百百": 0} {
		if got := spokenOrdinal(text); got != want {
			t.Errorf("%s: got %d want %d", text, got, want)
		}
	}
	s := retrievalFixture("这是普通的新增经历。")
	if got := modelContext(t, s); len(got.ContextTargets) != 0 {
		t.Fatal("invented target")
	}
}
