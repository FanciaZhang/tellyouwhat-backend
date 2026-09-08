package prompteval

import (
	"context"
	"encoding/json"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestJudgeAnonymousMappingAndMalformedScores(t *testing.T) {
	p := testPlan(t)
	p.Candidates = append(p.Candidates, p.Candidates[0])
	p.Candidates[0].Revision.ID = "private-current"
	p.Candidates[1].Revision.ID = "private-candidate"
	outputs := []Output{{Candidate: 0, Model: "private-model-a", Text: "baseline", Request: p.Requests[0]}, {Candidate: 1, Model: "private-model-b", Text: "candidate", Request: p.Requests[0]}}
	for _, mode := range []string{"valid", "missing_score", "negative_usage", "duplicate", "refused"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				input := body["input"].(string)
				if strings.Contains(input, "private-") {
					t.Error("judge received candidate identity")
				}
				var anonymous struct {
					Candidates []struct {
						Candidate int
						Output    string
					}
				}
				_ = json.Unmarshal([]byte(input), &anonymous)
				scores := []map[string]any{}
				for _, entry := range anonymous.Candidates {
					score := 4
					if entry.Output == "baseline" {
						score = 2
					}
					criterion := map[string]any{"score": score, "evidence": "合成理由"}
					if mode == "missing_score" {
						delete(criterion, "score")
					}
					index := entry.Candidate
					if mode == "duplicate" {
						index = 0
					}
					scores = append(scores, map[string]any{"candidate": index, "fidelity": criterion, "preservation": criterion, "intent": criterion, "style": criterion})
				}
				raw, _ := json.Marshal(map[string]any{"scores": scores})
				usage := 100
				if mode == "negative_usage" {
					usage = -1
				}
				kind := "output_text"
				if mode == "refused" {
					kind = "refusal"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "model": "actual-judge", "usage": map[string]int{"input_tokens": usage, "output_tokens": 100}, "output": []any{map[string]any{"content": []any{map[string]any{"type": kind, "text": string(raw)}}}}})
			}))
			defer server.Close()
			budget, _ := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 100e9, MaxConcurrent: 2, LeaseDuration: time.Hour}, time.Now)
			e := Engine{Provider: provider.Config{BaseURL: server.URL}}
			result := e.judge(context.Background(), budget, p, p.Samples[0], outputs)
			if mode != "valid" {
				if result.Status != "incomplete" {
					t.Fatal("malformed judge claimed completion", result)
				}
				return
			}
			if result.Status != "completed" || result.Model != "actual-judge" {
				t.Fatal(result)
			}
			for _, s := range result.Scores {
				want := 4
				if s.Candidate == 0 {
					want = 2
				}
				if s.Fidelity.Score != want {
					t.Fatal("anonymous mapping lost", result)
				}
			}
		})
	}
}
