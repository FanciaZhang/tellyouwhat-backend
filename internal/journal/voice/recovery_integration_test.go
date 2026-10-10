package voice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

// Explicit real-model checks use synthetic hesitant speech, never user audio.
// Every response is retained, including rejected responses; no success retries.
func TestConfiguredHesitantNarrativeRecovery(t *testing.T) {
	path := os.Getenv("JOURNAL_RECOVERY_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("explicit synthetic real-model recovery check only")
	}
	data, err := os.ReadFile(path)
	var config map[string]string
	if err != nil || json.Unmarshal(data, &config) != nil || config["JOURNAL_ARK_API_KEY"] == "" {
		t.Fatal("private configuration required")
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 200 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	model := NewBudgetedRewriter(ArkRewriter{BaseURL: config["JOURNAL_ARK_BASE_URL"], APIKey: config["JOURNAL_ARK_API_KEY"], Model: config["JOURNAL_VOICE_MODEL"]}, budget, "journal-development", costcontrol.TokenPrice{InputNanosPerMillionTokens: 2_000_000_000, OutputNanosPerMillionTokens: 12_000_000_000})
	author := uuid.NewString()
	for i := 0; i < 6; i++ {
		texts := []string{
			"我在讨论网站链接的风险判断。呃，就是用户没有打开网页，我们也希望提前发现危险链接，这个能力还在讨论，不是已经实现了。",
			"嗯，对。",
			"链接要经过我们自己的系统。我们可以先分析来源、域名和访问行为，不接触网页正文。我觉得这是一种感知能力，但还需要验证。",
			"这么一个广泛的一个。",
			"嗯。",
			"除此之外还有其他信号，比如说刚才说到那个，啊我还没有想清楚，不能现在就说一定能识别。最后我们决定先做小范围验证。",
		}
		required := []string{"风险", "域名", "访问", "验证", "想清楚", "实现"}
		if i == 4 {
			texts = []string{"我在讨论网站链接风险，先检查域名和访问行为。", "我有个朋友叫小林。", "接着说网站，能力还在讨论，尚未实现。"}
			required = []string{"域名", "访问", "小林", "实现"}
		}
		if i == 5 {
			texts = []string{"嗯，我还没想清楚。嗯，可能明天才确定。", "另外，今天我的脚也疼。", "我还想喝拿铁。"}
			required = []string{"想清楚", "可能", "明天", "脚", "疼", "拿铁"}
		}
		p := &PolishRequest{Narrator: &Narrator{PersonID: author, Name: "我"}}
		s := Snapshot{DictationMode: true, WritingStyle: "natural", Narrator: p.Narrator, Polish: p}
		for n, text := range texts {
			turn := SourceUtterance{ID: uuid.NewString(), Text: text, Speaker: "1"}
			if n != 3 {
				turn.PersonID, turn.Person = author, "我"
			}
			target := PolishTarget{ID: uuid.NewString(), Text: text, SourceText: text, Style: "body", CompleteSource: true, SourceIDs: []string{turn.ID}, Turns: []SourceUtterance{turn}}
			p.Targets = append(p.Targets, target)
			s.Blocks = append(s.Blocks, Block{ID: target.ID, Text: text, Style: "body"})
		}
		if i < 4 && config["InputPath"] != "" {
			input, err := os.ReadFile(config["InputPath"])
			if err != nil || json.Unmarshal(input, &s) != nil || s.Validate() != nil {
				t.Fatal("invalid actual synthetic App snapshot")
			}
			required = []string{"风险", "域名", "访问", "验证", "实现"}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
		result, callErr := model.Rewrite(ctx, s, 1)
		cancel()
		record := map[string]any{"synthetic": true, "appAcceptance": false, "snapshot": s, "raw": result.OutputText, "revision": result.Polish, "diagnostics": result.Diagnostics, "model": result.Model}
		if callErr != nil {
			record["error"] = callErr.Error()
		}
		data, err = json.MarshalIndent(record, "", "  ")
		if err != nil || os.WriteFile(filepath.Join(config["OutputPath"], "hesitant-"+string(rune('0'+i))+".json"), data, 0600) != nil {
			t.Fatal("preserve evidence")
		}
		if callErr != nil || result.Polish == nil {
			t.Errorf("attempt %d rejected at %s; synthetic response preserved", i, result.Diagnostics.Stage)
			continue
		}
		prose := ""
		for _, p := range result.Polish.Paragraphs {
			prose += p.Text
		}
		for _, fact := range required {
			if !strings.Contains(prose, fact) {
				t.Errorf("attempt %d lost %s; all response evidence retained", i, fact)
			}
		}
		if strings.Contains(prose, "这么一个广泛的一个") {
			t.Errorf("attempt %d did not remove a meaningless fragment", i)
		}
		if i >= 4 && len(result.Polish.Omissions) > 0 {
			t.Errorf("attempt %d excluded an intentional fact or uncertain thought", i)
		}
	}
}
