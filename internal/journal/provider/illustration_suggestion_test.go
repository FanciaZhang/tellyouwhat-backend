package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
)

func TestOrganizeAdviceIsGroundedOptionalAndDoesNotBreakCoreResult(t *testing.T) {
	valid := map[string]any{"summary": "雪地里的小狗", "subject": "一只小狗初次踩进雪地", "setting": "雪地", "composition": "留白", "style": "水彩", "sourceQuotes": []string{"小狗第一次踩进雪里"}, "reason": "具体的生活场景"}
	for _, tc := range []struct {
		name          string
		scene         any
		enabled, want bool
	}{
		{"grounded", valid, true, true}, {"disabled", valid, false, false}, {"none", nil, true, false},
		{"wrong type", "broken", true, false}, {"bad fields", map[string]any{"summary": 3}, true, false},
		{"invented source", map[string]any{"summary": "雨里的猫", "subject": "猫", "setting": "雨天", "composition": "", "style": "", "sourceQuotes": []string{"下雨了"}, "reason": "场景"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				_ = json.NewDecoder(r.Body).Decode(&payload)
				var input map[string]any
				_ = json.Unmarshal([]byte(payload["input"].(string)), &input)
				if input["illustrationSuggestionsEnabled"] != tc.enabled {
					t.Error("suggestion preference not sent")
				}
				output, _ := json.Marshal(map[string]any{"tags": []map[string]string{{"name": "小狗", "type": "topic"}}, "existingBookRecommendations": []any{}, "newBookSuggestions": []any{}, "illustrationSuggestion": tc.scene})
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}})
			}))
			defer server.Close()
			client := New(Config{BaseURL: server.URL, APIKey: "fixture", LiteModel: "fixture", ProModel: "fixture"}, server.Client())
			result, err := client.Organize(context.Background(), contracts.OrganizeRequest{Body: "小狗第一次踩进雪里。", IllustrationSuggestionsEnabled: tc.enabled}, false)
			if err != nil || len(result.Value.Tags) != 1 {
				t.Fatalf("core result lost: %+v %v", result, err)
			}
			if (result.Value.IllustrationSuggestion != nil) != tc.want {
				t.Fatal("incorrect advice admission")
			}
		})
	}
}
