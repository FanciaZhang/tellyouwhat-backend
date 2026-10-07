package voice

import (
	"context"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"
)

// Same discourse markers, different intentions. Real-model opt-in acceptance;
// no regex makes the semantic decision and no user journal is transmitted.
func TestLiveSemanticLists(t *testing.T) {
	if os.Getenv("JOURNAL_SEMANTIC_LISTS_LIVE_CHECK") != "1" {
		t.Skip("explicit synthetic service acceptance")
	}
	model := developmentEditRewriter{base: os.Getenv("JOURNAL_EDIT_INTENT_SERVICE_URL")}
	for _, tc := range []struct {
		name, text, retained string
		context              []Block
		items                int
		conclusion           string
		facts                []string
	}{
		{"inline_shopping", "今天买了苹果、牛奶和面包。", "", nil, 0, "", []string{"苹果", "牛奶", "面包"}},
		{"inline_packing", "周末出门带上水杯、雨伞、充电宝就够了。", "", nil, 0, "", []string{"水杯", "雨伞", "充电宝"}},
		{"inline_feelings", "今天觉得累、困、没精神，早点睡吧。", "", nil, 0, "", []string{"累", "困", "没精神", "睡"}},
		{"parallel_reasons", "首先价格低，其次离家近，最后环境好。", "", nil, 3, "", []string{"价格", "离家", "环境"}},
		{"steps", "做饭分三步，首先洗菜，其次切菜，最后下锅。", "", nil, 3, "", []string{"洗菜", "切菜", "下锅"}},
		{"experience", "今天首先去了医院，随后回家，最后终于能休息了。", "", nil, 0, "", []string{"医院", "回家", "休息"}},
		{"reasons_then_decision", "首先价格低，其次离家近。最后，我还是决定选这里。", "", nil, 2, "决定", []string{"价格", "离家", "这里"}},
		{"continued_draft", "其次离家近。最后，我还是决定选这里。", "价格低。", nil, 2, "决定", []string{"价格", "离家", "这里"}},
		{"continued_context", "其次离家近。最后环境好。", "", []Block{{ID: uuid.NewString(), Text: "价格低。", Style: "orderedListItem"}}, 2, "", []string{"离家", "环境"}},
		{"holdout_reasons", "我比较看重这几个方面。首先能步行到地铁站，其次楼下有菜市场。最后想来想去，还是暂时不搬家了。", "", nil, 2, "不搬", []string{"地铁", "菜市场", "不搬"}},
		{"holdout_steps", "清洗滤网时首先拔掉插头，其次拆下滤网，最后用清水冲洗。", "", nil, 3, "", []string{"插头", "滤网", "清水"}},
		{"single_emphasis", "选车时我首先考虑安全，其他方面还没有想好。", "", nil, 0, "", []string{"安全", "没有想好"}},
		{"meaningful_last", "有两件事要记住：首先，赶上最后一班车。其次，带上第一天要用的证件。", "", nil, 2, "", []string{"最后一班车", "第一天", "证件"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := polishFixture()
			s.WritingStyle = StyleNatural
			target := &s.Polish.Targets[0]
			target.Text = tc.retained + tc.text
			target.SourceText = tc.text
			target.RetainedText = tc.retained
			if tc.retained != "" {
				target.Style = "orderedListItem"
			}
			s.Blocks[0].Text = target.Text
			s.Blocks[0].Style = target.Style
			s.Polish.Context = tc.context
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
			all := ""
			for _, p := range result.Polish.Paragraphs {
				t.Logf("%s: %s", p.Style, p.Text)
				all += p.Text
				if p.Style == "orderedListItem" {
					count++
					for _, lead := range []string{"首先", "其次", "最后，", "最后下锅", "第一，", "第二，"} {
						if strings.HasPrefix(p.Text, lead) {
							t.Fatalf("organizational prefix remains: %q", p.Text)
						}
					}
				} else if p.Style != "body" {
					t.Fatalf("unexpected structure: %s", p.Style)
				}
			}
			if count != tc.items {
				t.Fatalf("want %d list items, got %d", tc.items, count)
			}
			for _, fact := range tc.facts {
				if !strings.Contains(all, fact) {
					t.Fatalf("lost fact %q: %s", fact, all)
				}
			}
			if tc.conclusion != "" {
				last := result.Polish.Paragraphs[len(result.Polish.Paragraphs)-1]
				if last.Style != "body" || !strings.Contains(last.Text, tc.conclusion) {
					t.Fatal("decision became a list item")
				}
			}
		})
	}
}
