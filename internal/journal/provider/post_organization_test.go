package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func TestPostOrganizationReturnsTitleAndSceneInOneCall(t *testing.T) {
	scene := map[string]any{"summary": "窗边读书", "subject": "打开的书和茶杯", "setting": "安静的房间", "composition": "近景留白", "style": "水彩", "sourceQuotes": []string{"我在窗边读完最后一页，茶还温着。"}, "reason": "具体的阅读时刻"}
	for _, tc := range []struct {
		name                string
		title, illustration bool
		result              map[string]any
		invalid             bool
	}{
		{"both", true, true, map[string]any{"title": "茶还温着", "illustrationSuggestion": scene}, false},
		{"title only", true, false, map[string]any{"title": "窗边的最后一页", "illustrationSuggestion": nil}, false},
		{"scene only", false, true, map[string]any{"title": nil, "illustrationSuggestion": scene}, false},
		{"no material", true, true, map[string]any{"title": nil, "illustrationSuggestion": nil}, false},
		{"missing title", true, true, map[string]any{"illustrationSuggestion": nil}, true},
		{"unrequested title", false, true, map[string]any{"title": "不应返回", "illustrationSuggestion": nil}, true},
		{"unrequested scene", true, false, map[string]any{"title": "窗边", "illustrationSuggestion": scene}, true},
		{"injected field", true, true, map[string]any{"title": "窗边", "illustrationSuggestion": nil, "body": "改写正文"}, true},
		{"multiline title", true, false, map[string]any{"title": "窗边\n读书", "illustrationSuggestion": nil}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var payload map[string]any
				_ = json.NewDecoder(r.Body).Decode(&payload)
				var input map[string]any
				_ = json.Unmarshal([]byte(payload["input"].(string)), &input)
				if input["generateTitle"] != tc.title || input["illustrationSuggestionsEnabled"] != tc.illustration {
					t.Error("request switches lost")
				}
				instructions := payload["instructions"].(string)
				if tc.title && !strings.Contains(instructions, promptconfig.EntryTitlePrompt) {
					t.Error("title policy missing")
				}
				format := payload["text"].(map[string]any)["format"].(map[string]any)
				if format["name"] != "journal_post_organization" {
					t.Error("separate model call used")
				}
				data, _ := json.Marshal(tc.result)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 30, "output_tokens": 20}, "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(data)}}}}})
			}))
			defer upstream.Close()
			client := New(Config{BaseURL: upstream.URL, APIKey: "fixture", LiteModel: "fixture", ProModel: "fixture"}, upstream.Client())
			result, err := client.Organize(context.Background(), contracts.OrganizeRequest{PostOrganization: true, GenerateTitle: tc.title, IllustrationSuggestionsEnabled: tc.illustration, Body: "我在窗边读完最后一页，茶还温着。"}, false)
			if tc.invalid {
				if !errors.Is(err, ErrInvalidResult) {
					t.Fatalf("invalid result accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if (result.Value.SuggestedTitle != nil) != (tc.result["title"] != nil) || (result.Value.IllustrationSuggestion != nil) != (tc.result["illustrationSuggestion"] != nil) {
				t.Fatal("combined result lost")
			}
			if calls != 1 {
				t.Fatalf("got %d model requests, want one", calls)
			}
		})
	}
}
