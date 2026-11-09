package voice

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

// Explicitly enabled synthetic semantic checks; these exercise the actual
// configured model. App capture, application and UI are separate acceptance.
func TestConfiguredNarrativePreservesActorsAndReciprocalPerspective(t *testing.T) {
	path := os.Getenv("JOURNAL_NARRATOR_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("explicit real configured narrative integration only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private integration configuration")
	}
	var config map[string]string
	if json.Unmarshal(raw, &config) != nil {
		t.Fatal("invalid integration configuration")
	}
	if config["JOURNAL_ARK_API_KEY"] == "" || config["JOURNAL_VOICE_MODEL"] == "" {
		t.Fatal("actual configured model required")
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 200 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	model := NewBudgetedRewriter(ArkRewriter{BaseURL: config["JOURNAL_ARK_BASE_URL"], APIKey: config["JOURNAL_ARK_API_KEY"], Model: config["JOURNAL_VOICE_MODEL"]}, budget,
		"journal-development", costcontrol.TokenPrice{InputNanosPerMillionTokens: 2_000_000_000, OutputNanosPerMillionTokens: 12_000_000_000})
	author, other := uuid.NewString(), uuid.NewString()
	target := func(personID, name, text, old string) PolishTarget {
		source := uuid.NewString()
		return PolishTarget{ID: uuid.NewString(), Text: old, SourceText: text, RetainedText: old, Style: "body", CompleteSource: true,
			SourceIDs: []string{source}, Turns: []SourceUtterance{{ID: source, Text: text, PersonID: personID, Person: name}}}
	}
	cases := []struct {
		name             string
		targets          []PolishTarget
		facts, forbidden []string
	}{
		{"reciprocal", []PolishTarget{
			target(author, "老公宝", "我和我老婆宝今天沿着河边散步，看到很多柳树，心情特别好。", "今天我和老婆宝去河边。"),
			target(other, "老婆宝", "对，我和我老公宝一起出来很开心。我昨天在医院值班，很累，今天终于能休息。", "老婆宝说和我老公宝出来开心。")},
			[]string{"老婆宝", "柳树", "医院", "累"}, []string{"和我老公宝", "我昨天在医院"}},
		{"same-speaker-actions", []PolishTarget{
			target(author, "老公宝", "我负责炒菜，盐放少了，我又补了一点。", "我负责炒菜。"),
			target(other, "老婆宝", "最后的味道挺好，我吃了两碗米饭，还把剩下的菜装进饭盒。", "老婆宝吃了两碗米饭，我把剩菜装进饭盒。"),
			target(author, "老公宝", "吃完饭以后，我们一起收拾桌子。", "吃完饭以后，我们一起收拾桌子。")},
			[]string{"老婆宝", "两碗", "饭盒", "收拾桌子"}, []string{"我吃了两碗", "我把剩", "我还把剩"}},
		{"one-speaker-many-action-subjects", []PolishTarget{
			target(other, "小林", "我叫小林。我昨天帮小王打了报告。小王有紧急任务，今天还要加班。", "小林昨天帮小王打了报告。小王有紧急任务，他今天还要加班。")},
			[]string{"小林", "小王", "报告", "加班"}, []string{"小林今天还要加班", "我今天还要加班"}},
		{"relevant-lighting-and-author-topic-change", []PolishTarget{
			target(author, "老公宝", "我和老婆宝挑选台灯。我们打开灯试了亮度，发现太暗，决定换一盏。", ""),
			target(other, "老婆宝", "对，我也觉得这盏灯太暗，要换一盏。", ""),
			target(author, "老公宝", "另外，我今天还想去河边散步，晚点喝杯拿铁。", "")},
			[]string{"亮度", "太暗", "换", "河边", "拿铁"}, []string{}},
	}
	records := []map[string]any{}
	defer func() {
		data, e := json.MarshalIndent(map[string]any{"synthetic": true, "appAcceptance": false, "attempts": records}, "", "  ")
		if e == nil {
			e = os.WriteFile(config["OutputPath"], data, 0600)
		}
		if e != nil {
			t.Error("could not preserve semantic integration result")
		}
	}()
	for _, c := range cases {
		for repetition := 0; repetition < 3; repetition++ {
			p := &PolishRequest{Narrator: &Narrator{PersonID: author, Name: "老公宝"}, Targets: c.targets, Context: []Block{}}
			if c.name == "relevant-lighting-and-author-topic-change" {
				p.SelectionContext = &PolishSelectionContext{Opening: "我和老婆宝之前商量买洗碗机。", Omissions: []PolishOmission{{SourceID: uuid.NewString(), Text: "老张，把三楼办公室的灯关掉。", Reason: "backgroundConversation"}}}
			}
			blocks := []Block{}
			for _, v := range c.targets {
				blocks = append(blocks, Block{ID: v.ID, Text: v.Text, Style: v.Style})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			result, e := model.Rewrite(ctx, Snapshot{DictationMode: true, WritingStyle: "natural", Polish: p, Narrator: p.Narrator, Blocks: blocks}, 1)
			cancel()
			text := ""
			if result.Polish != nil {
				for _, paragraph := range result.Polish.Paragraphs {
					text += paragraph.Text + "\n"
				}
			}
			records = append(records, map[string]any{"case": c.name, "repetition": repetition, "text": text, "output": result.OutputText,
				"model": result.Model, "inputTokens": result.InputTokens, "outputTokens": result.OutputTokens, "stage": result.Diagnostics.Stage, "success": e == nil})
			if e != nil || result.Polish == nil {
				t.Errorf("%s-%d real model failed; response preserved", c.name, repetition)
				continue
			}
			for _, fact := range c.facts {
				present := strings.Contains(text, fact)
				if fact == "收拾桌子" {
					present = regexp.MustCompile(`收拾(?:了)?桌子`).MatchString(text)
				}
				if !present {
					t.Errorf("%s-%d lost %s; response preserved", c.name, repetition, fact)
				}
			}
			if c.name == "one-speaker-many-action-subjects" && !regexp.MustCompile(`小王[^。！？]*加班`).MatchString(text) {
				t.Errorf("%s-%d third-party action attribution lost; response preserved", c.name, repetition)
			}
			for _, phrase := range c.forbidden {
				if strings.Contains(text, phrase) {
					t.Errorf("%s-%d incorrect actor %s; response preserved", c.name, repetition, phrase)
				}
			}
			if c.name == "same-speaker-actions" && !regexp.MustCompile(`老婆宝[^。！？]*饭盒`).MatchString(text) {
				t.Errorf("%s-%d packing actor missing; response preserved", c.name, repetition)
			}
		}
	}
}
