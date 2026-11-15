package promptconfig

import (
	"context"
	"strings"
	"testing"
)

func TestEntryTitlePolicyIsEditableAndGenreFlexible(t *testing.T) {
	for _, rule := range []string{"摘取", "概括", "意象", "没有固定模板", "不制造悬念", "不补写", "null"} {
		if !strings.Contains(EntryTitlePrompt, rule) {
			t.Fatalf("missing title principle %s", rule)
		}
	}
	p := Defaults("lite", "pro", "voice", 60)["journal"]
	if err := p.Validate("journal"); err != nil {
		t.Fatal(err)
	}
	p.Journal.EntryTitlePrompt = "按正文取一个自然的标题。"
	ctx := WithRevision(context.Background(), Revision{Policy: p})
	if TitlePrompt(ctx) != p.Journal.EntryTitlePrompt {
		t.Fatal("published title policy ignored")
	}
	p.Journal.EntryTitlePrompt = ""
	if TitlePrompt(WithRevision(context.Background(), Revision{Policy: p})) != EntryTitlePrompt {
		t.Fatal("default title policy unavailable")
	}
}
