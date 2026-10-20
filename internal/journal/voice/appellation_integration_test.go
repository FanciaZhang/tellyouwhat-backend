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

// Explicit synthetic audio and the installed service's provider configuration;
// records are real provider output, separately replayed by the App acceptance.
func TestConfiguredAppellationWording(t *testing.T) {
	path := os.Getenv("JOURNAL_APPELLATION_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("explicit synthetic real-provider appellation integration only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private integration configuration unavailable")
	}
	var config map[string]string
	if json.Unmarshal(raw, &config) != nil || config["JOURNAL_ARK_API_KEY"] == "" || config["JOURNAL_VOICE_MODEL"] == "" || config["OutputPath"] == "" {
		t.Fatal("actual configured model and evidence destination required")
	}
	model := ArkRewriter{BaseURL: config["JOURNAL_ARK_BASE_URL"], APIKey: config["JOURNAL_ARK_API_KEY"], Model: config["JOURNAL_VOICE_MODEL"]}
	records := []map[string]any{}
	defer func() {
		data, err := json.MarshalIndent(map[string]any{"synthetic": true, "attempts": records}, "", "  ")
		if err != nil || os.WriteFile(config["OutputPath"], data, 0600) != nil {
			t.Error("could not preserve provider evidence")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pcm, err := os.ReadFile(config["PCMPath"])
	if err != nil || len(pcm) == 0 || len(pcm)%2 != 0 {
		t.Fatal("synthetic PCM16 required")
	}
	asr := ASR{Config: ASRConfig{URL: "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async", ResourceID: "volc.seedasr.sauc.duration", StreamInsights: true,
		APIKey: config["JOURNAL_VOICE_ASR_API_KEY"], AppKey: config["JOURNAL_VOICE_ASR_APP_KEY"], AccessKey: config["JOURNAL_VOICE_ASR_ACCESS_KEY"]}}
	connection, err := asr.Open(ctx, nil)
	if err != nil {
		t.Fatal("configured ASR open failed", err)
	}
	defer connection.Close()
	type completion struct {
		transcript Transcript
		err        error
	}
	done := make(chan completion, 1)
	go func() {
		for {
			v, err := connection.Receive()
			if err != nil || v.Final {
				done <- completion{v, err}
				return
			}
		}
	}()
	for offset := 0; offset < len(pcm); offset += 6400 {
		end := min(offset+6400, len(pcm))
		if err := connection.Send(pcm[offset:end], end == len(pcm)); err != nil {
			t.Fatal("configured ASR send failed", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	var recognized completion
	select {
	case recognized = <-done:
	case <-ctx.Done():
		t.Fatal("configured ASR did not finish")
	}
	records = append(records, map[string]any{"stage": "asr", "transcript": recognized.transcript, "success": recognized.err == nil})
	if recognized.err != nil || !strings.Contains(recognized.transcript.Text, "老婆") || strings.Contains(recognized.transcript.Text, "老婆宝") {
		t.Fatal("synthetic source must contain the spoken appellation; actual ASR evidence preserved", recognized.err)
	}
	cases := []struct {
		name, text, expected string
		forbidden            []string
		words                []string
	}{
		{"spoken-wife", recognized.transcript.Text, "老婆", []string{"老婆宝", "妻子", "爱人", "老公宝"}, nil},
		{"wife", "今天和妻子去图书馆借了一本菜谱，回家后妻子照着菜谱做了面包。", "妻子", []string{"老婆", "爱人", "小岚"}, nil},
		{"partner", "今天爱人买回了草莓，我们一起在阳台上吃草莓，看着雨停下来。", "爱人", []string{"老婆", "妻子", "小岚"}, nil},
		{"given-name", "今天我和小岚一起坐公交车去海边，小岚拍到了橙色的晚霞。", "小岚", []string{"老婆", "妻子", "爱人"}, nil},
		{"original-nickname", "今天和老婆宝去逛花市，老婆宝挑了一盆茉莉花，回家放在窗台上。", "老婆宝", []string{"妻子", "爱人", "小岚"}, nil},
		{"vocabulary-is-not-rename", "今天和老婆去菜市场买番茄，回家后老婆做了番茄炒蛋。", "老婆", []string{"老婆宝", "老公宝"}, []string{"老婆宝", "老公宝"}},
	}
	for _, c := range cases {
		for repetition := 0; repetition < 2; repetition++ {
			t.Run(c.name+string(rune('0'+repetition)), func(t *testing.T) {
				author, source, block := uuid.NewString(), uuid.NewString(), uuid.NewString()
				p := &PolishRequest{Narrator: &Narrator{PersonID: author, Name: "我"}, Context: []Block{}, Targets: []PolishTarget{{ID: block, Text: c.text, SourceText: c.text, Style: "body", CompleteSource: true,
					SourceIDs: []string{source}, Turns: []SourceUtterance{{ID: source, Text: c.text, PersonID: author, Person: "我"}}}}}
				result, err := model.Rewrite(ctx, Snapshot{DictationMode: true, WritingStyle: "natural", Words: c.words, Polish: p, Narrator: p.Narrator, Blocks: []Block{{ID: block, Text: c.text, Style: "body"}}}, 1)
				text := ""
				if result.Polish != nil {
					for _, paragraph := range result.Polish.Paragraphs {
						text += paragraph.Text + "\n"
					}
				}
				records = append(records, map[string]any{"case": c.name, "repetition": repetition, "source": c.text, "expected": c.expected, "request": p, "revision": result.Polish,
					"text": text, "raw": result.OutputText, "model": result.Model, "success": err == nil})
				if err != nil || result.Polish == nil {
					t.Fatal("configured rewrite failed; actual response preserved", err)
				}
				if !strings.Contains(text, c.expected) {
					t.Errorf("original appellation lost: %s", text)
				}
				for _, word := range c.forbidden {
					if strings.Contains(text, word) {
						t.Errorf("appellation changed without a rename: %s", text)
					}
				}
			})
		}
	}
}
