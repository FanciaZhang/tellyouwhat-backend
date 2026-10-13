package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
)

func TestDedicatedIllustrationSuggestionIsGroundedAndSeparateFromOrganization(t *testing.T) {
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
				if (input["illustrationSuggestionsEnabled"] == true) != tc.enabled {
					t.Error("suggestion preference not sent")
				}
				value := map[string]any{"illustrationSuggestion": tc.scene}
				if !tc.enabled {
					value = map[string]any{"tags": []map[string]string{{"name": "小狗", "type": "topic"}}, "existingBookRecommendations": []any{}, "newBookSuggestions": []any{}}
				}
				format := payload["text"].(map[string]any)["format"].(map[string]any)
				properties := format["schema"].(map[string]any)["properties"].(map[string]any)
				if tc.enabled && (properties["tags"] != nil || input["books"] != nil) {
					t.Error("scene request mixed organization concerns")
				}
				if !tc.enabled && properties["illustrationSuggestion"] != nil {
					t.Error("organization still requests scenes")
				}
				output, _ := json.Marshal(value)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}})
			}))
			defer server.Close()
			client := New(Config{BaseURL: server.URL, APIKey: "fixture", LiteModel: "fixture", ProModel: "fixture"}, server.Client())
			result, err := client.Organize(context.Background(), contracts.OrganizeRequest{Body: "小狗第一次踩进雪里。", IllustrationSuggestionsEnabled: tc.enabled}, false)
			if err != nil || tc.enabled && len(result.Value.Tags) != 0 || !tc.enabled && len(result.Value.Tags) != 1 {
				t.Fatalf("core result lost: %+v %v", result, err)
			}
			if (result.Value.IllustrationSuggestion != nil) != tc.want {
				t.Fatal("incorrect advice admission")
			}
		})
	}
}
