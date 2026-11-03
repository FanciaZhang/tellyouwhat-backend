package voice

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"os"
	"strings"
	"testing"
	"time"
)

// Real model checks use only synthetic utterances and explicitly supplied
// private configuration. Capture/application/UI require separate App checks.
func TestConfiguredSituationalNarrativeSelection(t *testing.T) {
	path := os.Getenv("JOURNAL_SITUATIONAL_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("explicit real-model situational selection only")
	}
	data, err := os.ReadFile(path)
	var config map[string]string
	if err != nil || json.Unmarshal(data, &config) != nil || config["JOURNAL_ARK_API_KEY"] == "" || config["JOURNAL_VOICE_MODEL"] == "" {
		t.Fatal("private actual-model configuration required")
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 200 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	model := NewBudgetedRewriter(ArkRewriter{BaseURL: config["JOURNAL_ARK_BASE_URL"], APIKey: config["JOURNAL_ARK_API_KEY"], Model: config["JOURNAL_VOICE_MODEL"]}, budget, "journal-development", costcontrol.TokenPrice{InputNanosPerMillionTokens: 2_000_000_000, OutputNanosPerMillionTokens: 12_000_000_000})
	author, wife := uuid.NewString(), uuid.NewString()
	type utterance struct{ person, text string }
	cases := []struct {
		name             string
		turns            []utterance
		omitted          []int
		facts, forbidden []string
	}{
		{"author-and-known-person-interruptions", []utterance{
			{author, "我和老婆宝在讨论工作报告，客户要求周五交付，我负责核对数据。"},
			{wife, "报告里的图表也要检查，不能把错误数据交给客户。"},
			{author, "哎哟，我脚崴了，好疼。"},
			{wife, "啊，鞋里进沙子了，硌得慌。"},
			{author, "接着说报告，明天下午把草稿发给同事，周五准时交给客户。"}},
			[]int{2, 3}, []string{"周五", "数据", "图表", "明天下午", "同事"}, []string{"脚崴", "好疼", "沙子", "硌"}},
		{"injury-is-the-narrative", []utterance{
			{author, "我今天走路脚崴了，好疼，我想记下这次受伤。"},
			{wife, "我陪你去医院看看脚，别再走太多路。"},
			{author, "我打算先休息，明天再看疼痛有没有缓解。"}},
			nil, []string{"脚", "疼", "医院", "休息"}, nil},
		{"injury-changes-work-plan", []utterance{
			{author, "我在讲周五的工作报告。我脚崴了，好疼，没法到客户现场。"},
			{author, "因此把汇报改成线上，先把报告发给客户。"}},
			nil, []string{"脚", "疼", "线上", "客户"}, nil},
		{"reaction-adopted-by-following-memory", []utterance{
			{author, "我在说本周工作。哎哟，我脚崴了，好疼。"},
			{author, "这让我想起上次受伤的时候，同事帮我做完了报告。我一直很感激，这段也想记下来。"}},
			nil, []string{"脚", "疼", "上次", "同事", "感激"}, nil},
		{"deliberate-new-topic", []utterance{
			{author, "工作报告我准备周五交给客户。"},
			{author, "另外我想记一下今天脚崴了的事，好疼，得休息两天。这件事跟报告没有关系。"}},
			nil, []string{"周五", "脚", "疼", "两天"}, nil},
		{"ambiguous-new-topic-stays", []utterance{
			{author, "报告周五交。"}, {author, "另外今天我的脚也疼。"}},
			nil, []string{"周五", "脚", "疼"}, nil},
		{"negative-opinion-is-a-contribution", []utterance{
			{author, "客户想周五交报告。"},
			{wife, "我觉得这个期限很不合理，太赶了，容易出错。"},
			{author, "我还是先核对数据，再商量是否延期。"}},
			nil, []string{"周五", "不合理", "数据", "延期"}, nil},
		{"unannounced-plan-is-not-a-reaction", []utterance{
			{author, "报告要周五交，明天下午发草稿。"},
			{author, "我要去大东海。"},
			{author, "接着说工作报告，要核对数据。"}},
			nil, []string{"周五", "明天下午", "大东海", "数据"}, nil},
		{"mixed-turn-keeps-mainline", []utterance{
			{author, "工作报告周五交，先核对数据。哎哟，我脚崴了，好疼。接着说报告，明天下午发草稿给同事。"}},
			nil, []string{"周五", "数据", "明天下午", "草稿", "同事"}, []string{"脚崴", "好疼"}},
	}
	records := []map[string]any{}
	defer func() {
		data, e := json.MarshalIndent(map[string]any{"synthetic": true, "appAcceptance": false, "attempts": records}, "", "  ")
		if e != nil || os.WriteFile(config["OutputPath"], data, 0600) != nil {
			t.Error("preserve integration evidence")
		}
	}()
	for _, c := range cases {
		for repetition := 0; repetition < 2; repetition++ {
			targets := []PolishTarget{}
			for _, u := range c.turns {
				name := "老公宝"
				if u.person == wife {
					name = "老婆宝"
				}
				turn := SourceUtterance{ID: uuid.NewString(), Text: u.text, PersonID: u.person, Person: name}
				targets = append(targets, PolishTarget{ID: uuid.NewString(), Text: u.text, SourceText: u.text, Style: "body", CompleteSource: true, SourceIDs: []string{turn.ID}, Turns: []SourceUtterance{turn}})
			}
			p := &PolishRequest{Narrator: &Narrator{PersonID: author, Name: "老公宝"}, Targets: targets}
			snapshot := Snapshot{DictationMode: true, WritingStyle: "natural", Narrator: p.Narrator, Polish: p}
			for _, v := range targets {
				snapshot.Blocks = append(snapshot.Blocks, Block{ID: v.ID, Text: v.Text, Style: v.Style})
			}
			// Match the App's bounded recovery for invalid provider envelopes.
			// Never retry a semantically wrong but valid response to make its
			// assertions pass. Every rejected wire response stays in evidence.
			var result RewriteResult
			var e error
			for attempt := 0; attempt < 2; attempt++ {
				ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
				result, e = model.Rewrite(ctx, snapshot, 1)
				cancel()
				if e == nil {
					break
				}
				records = append(records, map[string]any{"case": c.name, "repetition": repetition, "rejectedAttempt": attempt, "snapshot": snapshot, "raw": result.OutputText, "error": e.Error(), "diagnostics": result.Diagnostics, "model": result.Model})
				t.Logf("%s-%d rejected provider result %d retained; bounded retry", c.name, repetition, attempt)
			}
			text := ""
			if result.Polish != nil {
				for _, v := range result.Polish.Paragraphs {
					text += v.Text + "\n"
				}
			}
			record := map[string]any{"case": c.name, "repetition": repetition, "snapshot": snapshot, "revision": result.Polish, "raw": result.OutputText, "text": text, "model": result.Model, "diagnostics": result.Diagnostics}
			if e != nil {
				record["error"] = e.Error()
			}
			records = append(records, record)
			if e != nil || result.Polish == nil {
				t.Errorf("%s-%d model failed; evidence saved", c.name, repetition)
				continue
			}
			for _, fact := range c.facts {
				if !strings.Contains(text, fact) {
					t.Errorf("%s-%d lost %s", c.name, repetition, fact)
				}
			}
			for _, phrase := range c.forbidden {
				if strings.Contains(text, phrase) {
					t.Errorf("%s-%d kept incidental %s", c.name, repetition, phrase)
				}
			}
			if len(result.Polish.Omissions) != len(c.omitted) {
				t.Errorf("%s-%d dispositions = %d, expected %d", c.name, repetition, len(result.Polish.Omissions), len(c.omitted))
			}
			for _, index := range c.omitted {
				found := false
				for _, o := range result.Polish.Omissions {
					if o.SourceID == targets[index].Turns[0].ID && o.Reason == "situationalInterruption" {
						found = true
					}
				}
				if !found {
					t.Errorf("%s-%d did not exclude known source %d as situational", c.name, repetition, index)
				}
			}
			t.Logf("%s-%d actual model completed; output preserved", c.name, repetition)
		}
	}
}
