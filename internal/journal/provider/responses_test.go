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
)

func TestProviderUsesAliasesAndDisablesStorage(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"tags\":[],\"existingBookRecommendations\":[{\"bookID\":\"b1\",\"reason\":\"相关\"}],\"newBookSuggestions\":[]}"}]}]}`))
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL, APIKey: "secret", LiteModel: "lite", ProModel: "pro"}, server.Client())
	result, err := client.Organize(context.Background(), contracts.OrganizeRequest{
		Title: "一天", Body: "正文", Books: []contracts.BookContext{{ID: "private-uuid", Name: "生活"}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if payload["store"] != false {
		t.Fatal("provider storage must be disabled")
	}
	thinking, ok := payload["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatal("journal organization must disable deep thinking")
	}
	textConfig, ok := payload["text"].(map[string]any)
	if !ok {
		t.Fatalf("provider text configuration is malformed: %#v", payload["text"])
	}
	format, ok := textConfig["format"].(map[string]any)
	if !ok || format["type"] != "json_schema" || format["strict"] != true {
		t.Fatalf("provider structured output is not strict JSON Schema: %#v", textConfig)
	}
	inputJSON, ok := payload["input"].(string)
	if !ok {
		t.Fatalf("provider input is not a JSON string: %#v", payload["input"])
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		t.Fatal(err)
	}
	if _, ok := input["existingTags"]; !ok {
		t.Fatalf("provider input does not use the public lower-camel contract: %#v", input)
	}
	books, ok := input["books"].([]any)
	if !ok || len(books) != 1 {
		t.Fatalf("provider books are malformed: %#v", input["books"])
	}
	book, ok := books[0].(map[string]any)
	if !ok || book["id"] != "b1" || book["name"] != "生活" {
		t.Fatalf("provider book alias is malformed: %#v", books[0])
	}
	encoded, _ := json.Marshal(payload["input"])
	if strings.Contains(string(encoded), "private-uuid") {
		t.Fatal("provider received persistent book id")
	}
	if result.Value.ExistingBookRecommendations[0].BookID != "private-uuid" {
		t.Fatal("book alias was not restored")
	}
}

func TestProviderRejectsTrailingStructuredOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"tags\":[],\"existingBookRecommendations\":[],\"newBookSuggestions\":[]} {}"}]}]}`))
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL, APIKey: "secret", LiteModel: "lite", ProModel: "pro"}, server.Client())
	_, err := client.Organize(context.Background(), contracts.OrganizeRequest{Title: "一天"}, false)
	if !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("expected invalid structured result, got %v", err)
	}
}

func TestProviderRejectsIncompleteResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"in_progress","output":[]}`))
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL, APIKey: "secret", LiteModel: "lite", ProModel: "pro"}, server.Client())
	_, err := client.Organize(context.Background(), contracts.OrganizeRequest{Title: "一天"}, false)
	if !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("expected incomplete response to be rejected, got %v", err)
	}
}
