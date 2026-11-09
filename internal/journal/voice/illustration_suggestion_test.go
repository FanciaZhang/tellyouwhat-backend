package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestVoiceAdviceFailureDoesNotDiscardRevision(t *testing.T) {
	for _, tc := range []struct {
		name          string
		scene         any
		enabled, want bool
	}{
		{"scene", map[string]any{"summary": "河岸的小狗", "subject": "小狗", "setting": "河岸", "composition": "", "style": "水彩", "sourceQuotes": []string{"河边的小狗"}, "reason": "完整场景"}, true, true},
		{"malformed", []any{3}, true, false}, {"none", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.NewString()
			snapshot := Snapshot{Revision: 3, Blocks: []Block{{ID: id, Text: "河边的小狗"}}, Transcript: "河边的小狗", IllustrationSuggestionsEnabled: tc.enabled}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				output, _ := json.Marshal(map[string]any{"baseRevision": 3, "transcriptRevision": 7, "patches": []any{}, "passages": []any{}, "questions": []any{}, "emotions": []any{}, "overallEmotion": "", "illustrationSuggestion": tc.scene})
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}})
			}))
			defer server.Close()
			result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "fixture", Model: "fixture"}).Rewrite(context.Background(), snapshot, 7)
			if err != nil || result.Revision.BaseRevision != 3 {
				t.Fatalf("core rewrite failed: %v", err)
			}
			if (result.Revision.IllustrationSuggestion != nil) != tc.want {
				t.Fatal("incorrect advice admission")
			}
		})
	}
}

func TestNarrativeAdviceRespectsSnapshotPreferenceAndPreservesPolishOnBadScene(t *testing.T) {
	scene := map[string]any{"summary": "河岸的小狗", "subject": "小狗", "setting": "河岸", "composition": "", "style": "水彩", "sourceQuotes": []string{"河边的小狗"}, "reason": "完整场景"}
	for _, tc := range []struct {
		name          string
		scene         any
		enabled, want bool
	}{
		{"enabled", scene, true, true}, {"disabled", scene, false, false},
		{"malformed", []any{3}, true, false}, {"none", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := polishFixture()
			snapshot.IllustrationSuggestionsEnabled = tc.enabled
			snapshot.Polish.Targets[0].Text = "河边的小狗"
			snapshot.Polish.Targets[0].SourceText = "河边的小狗"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					Input string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				var material struct {
					Enabled bool `json:"illustrationSuggestionsEnabled"`
				}
				if err := json.Unmarshal([]byte(input.Input), &material); err != nil || material.Enabled != tc.enabled {
					t.Error("narrative preference not supplied")
				}
				output, _ := json.Marshal(map[string]any{"paragraphs": []any{map[string]any{"text": "河边的小狗。", "style": "body", "targetIDs": []string{snapshot.Polish.Targets[0].ID}}}, "questions": []any{}, "illustrationSuggestion": tc.scene})
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}})
			}))
			defer server.Close()
			result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "fixture", Model: "fixture"}).Rewrite(context.Background(), snapshot, 7)
			if err != nil || result.Polish == nil || len(result.Polish.Paragraphs) != 1 {
				t.Fatalf("core narrative failed: %v", err)
			}
			if (result.Polish.IllustrationSuggestion != nil) != tc.want {
				t.Fatal("incorrect narrative advice admission")
			}
		})
	}
}
