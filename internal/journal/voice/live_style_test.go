package voice

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Explicit deployment acceptance, using invented journal text only. Reuse the
// current narrative request and acknowledgement protocol used by the App.
func TestLiveWritingStylesOnDevelopmentService(t *testing.T) {
	if os.Getenv("JOURNAL_WRITING_STYLE_LIVE_CHECK") != "1" {
		t.Skip("explicit live development acceptance only")
	}
	base := os.Getenv("JOURNAL_WRITING_STYLE_LIVE_URL")
	if base != "https://api-test.journal.tellyouwhat.cn" {
		t.Fatal("provide the isolated development URL")
	}
	data, err := os.ReadFile(os.Getenv("JOURNAL_WRITING_STYLE_SESSION_FILE"))
	if err != nil {
		t.Fatal("development session file unavailable")
	}
	var credential struct{ Token string }
	if json.Unmarshal(data, &credential) != nil || credential.Token == "" {
		t.Fatal("development credential unavailable")
	}
	t.Setenv("JOURNAL_DEVELOPMENT_TOKEN", credential.Token)
	model := developmentEditRewriter{base: base}
	for _, style := range []WritingStyle{StyleNatural, StyleLively, StyleDocumentary, StyleDaybook, StyleEssay} {
		t.Run(string(style), func(t *testing.T) {
			snapshot := polishFixture()
			snapshot.WritingStyle = style
			text := "下午三点我去河边走了走，买了杯热茶，坐了一会儿，心情轻松了些。对了，中午十二点还在公司附近吃了面。不是牛肉面，是番茄鸡蛋面。"
			snapshot.Blocks[0].Text = text
			snapshot.Polish.Targets[0].Text = text
			snapshot.Polish.Targets[0].SourceText = text
			snapshot.Polish.Context = []Block{{ID: uuid.NewString(), Text: "早上八点，我出门上班。", Style: "body"}, {ID: uuid.NewString(), Text: "晚上八点，我回到家。", Style: "body"}}
			if err := snapshot.Validate(); err != nil {
				t.Fatal("invalid synthetic narrative request", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			defer cancel()
			result, err := model.Rewrite(ctx, snapshot, 0)
			if err != nil || result.Polish == nil || len(result.Polish.Paragraphs) == 0 {
				t.Fatal("development narrative unavailable", err)
			}
			var paragraphs []string
			for _, paragraph := range result.Polish.Paragraphs {
				if len(paragraph.TargetIDs) != 1 || paragraph.TargetIDs[0] != snapshot.Blocks[0].ID {
					t.Fatal("narrative crossed into unchanged context")
				}
				paragraphs = append(paragraphs, paragraph.Text)
			}
			body := strings.Join(paragraphs, "\n")
			if !strings.Contains(body, "番茄鸡蛋面") || !strings.Contains(body, "河边") || !strings.Contains(body, "热茶") {
				t.Fatal("synthetic content was omitted")
			}
			if strings.Index(body, "番茄鸡蛋面") > strings.Index(body, "河边") {
				t.Fatal("late supplement was not put in chronological order")
			}
			t.Logf("Synthetic result (%s):\n%s", style, body)
		})
	}
}
