package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

// Explicit operator integration: synthetic text only, one attempt per cell and
// a separate conservative ceiling. It does not simulate a customer purchase.
func TestConfiguredVoiceOrganizationModelComparison(t *testing.T) {
	path := os.Getenv("JOURNAL_VOICE_MODEL_COMPARISON_CONFIG")
	if path == "" {
		t.Skip("explicit synthetic provider comparison only")
	}
	var config struct {
		BaseURL, APIKey, OutputDirectory string
		Candidates                       []struct {
			Name, Model       string
			Lightweight, Fast bool
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &config) != nil || config.APIKey == "" || len(config.Candidates) == 0 || len(config.Candidates) > 6 {
		t.Fatal("bounded private comparison configuration required")
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{
		MonthlyBudgetNanos: 5 * costcontrol.NanosPerCNY, MaxConcurrent: 1, LeaseDuration: 2 * time.Minute,
	}, time.Now)
	if err != nil {
		t.Fatal("comparison budget unavailable")
	}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 10_000_000_000, OutputNanosPerMillionTokens: 100_000_000_000}
	cases := []struct {
		name         string
		turns        []string
		required     []string
		otherSpeaker bool
	}{
		{"facts", []string{"今天下午3点，我和小林在青禾咖啡馆见面。原本说预算300元，后来我改成280元。", "钱还没付，不是已经付款。我可能周六出发，还没有决定。"}, []string{"小林", "青禾", "280", "没", "可能"}, false},
		{"speakers", []string{"今天我和妻子沿河散步，我说我今天没有去医院，去的是图书馆。", "我叫小夏，是他的妻子。我昨天去医院值班，觉得很累。", "后来我提醒妻子，我们明天下午3点还要一起去取书。"}, []string{"妻子", "小夏", "图书馆", "医院", "累", "取书"}, true},
		{"hesitation", []string{"这个能力还在讨论，不是已经实现了。", "嗯。", "先检查域名和访问行为，不能现在就说一定能识别。我还没想清楚，可能明天才确定。"}, []string{"讨论", "实现", "域名", "访问", "想清楚", "可能"}, false},
	}
	for _, candidate := range config.Candidates {
		if strings.ContainsAny(candidate.Name, "/\\") || candidate.Name == "" {
			t.Fatal("invalid evidence label")
		}
		for _, sample := range cases {
			s := comparisonSnapshot(sample.turns, sample.otherSpeaker)
			s.Polish.Lightweight = candidate.Lightweight
			policy := promptconfig.Defaults(candidate.Model, candidate.Model, candidate.Model, 90)["journal"]
			policy.Journal.Voice.Parameters.Price = &price
			policy.Journal.Organize.Lite.Price = &price
			ctx := promptconfig.WithRevision(t.Context(), promptconfig.Revision{Policy: policy})
			ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
			client := &http.Client{Transport: comparisonTier{fast: candidate.Fast}}
			model := NewBudgetedRewriter(ArkRewriter{BaseURL: config.BaseURL, APIKey: config.APIKey, Model: candidate.Model, HTTP: client}, budget, "journal-development-comparison", price)
			started := time.Now()
			result, callErr := model.Rewrite(ctx, s, 1)
			cancel()
			prose := ""
			if result.Polish != nil {
				for _, paragraph := range result.Polish.Paragraphs {
					prose += paragraph.Text
				}
			}
			missing := []string{}
			for _, fact := range sample.required {
				if !strings.Contains(prose, fact) {
					missing = append(missing, fact)
				}
			}
			record := map[string]any{"synthetic": true, "appAcceptance": false, "candidate": candidate.Name,
				"sample": sample.name, "model": result.Model, "fast": candidate.Fast, "lightweight": candidate.Lightweight,
				"elapsedMilliseconds": time.Since(started).Milliseconds(), "inputTokens": result.InputTokens, "outputTokens": result.OutputTokens,
				"snapshot": s, "raw": result.OutputText, "polish": result.Polish, "diagnostics": result.Diagnostics, "missingRequiredFacts": missing,
				"passed": callErr == nil && result.Polish != nil && len(missing) == 0}
			if callErr != nil {
				record["errorCode"] = rewriteClientCode(callErr)
			}
			encoded, _ := json.MarshalIndent(record, "", "  ")
			if os.WriteFile(filepath.Join(config.OutputDirectory, candidate.Name+"-"+sample.name+".json"), encoded, 0600) != nil {
				t.Fatal("comparison evidence could not be preserved")
			}
			t.Logf("candidate=%s sample=%s passed=%t duration_ms=%d stage=%s", candidate.Name, sample.name, record["passed"], record["elapsedMilliseconds"], result.Diagnostics.Stage)
		}
	}
}

func comparisonSnapshot(texts []string, other bool) Snapshot {
	author, companion := uuid.NewString(), uuid.NewString()
	s := Snapshot{DictationMode: true, WritingStyle: "natural", Narrator: &Narrator{PersonID: author, Name: "我"}}
	s.Polish = &PolishRequest{Narrator: s.Narrator}
	for i, text := range texts {
		turn := SourceUtterance{ID: uuid.NewString(), Text: text, Speaker: "speaker-1", PersonID: author, Person: "我"}
		if other && i == 1 {
			turn.Speaker, turn.PersonID, turn.Person = "speaker-2", companion, "妻子"
		}
		target := PolishTarget{ID: uuid.NewString(), Text: text, SourceText: text, Style: "body", SourceIDs: []string{turn.ID}, Turns: []SourceUtterance{turn}, CompleteSource: true}
		s.Polish.Targets = append(s.Polish.Targets, target)
		s.Blocks = append(s.Blocks, Block{ID: target.ID, Text: text, Style: "body"})
	}
	return s
}

type comparisonTier struct{ fast bool }

func (transport comparisonTier) RoundTrip(request *http.Request) (*http.Response, error) {
	if !transport.fast {
		return http.DefaultTransport.RoundTrip(request)
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	_ = request.Body.Close()
	var payload map[string]any
	if err = json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	payload["service_tier"] = "fast"
	raw, _ = json.Marshal(payload)
	copy := request.Clone(request.Context())
	copy.Body = io.NopCloser(bytes.NewReader(raw))
	copy.ContentLength = int64(len(raw))
	return http.DefaultTransport.RoundTrip(copy)
}
