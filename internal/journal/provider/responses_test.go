package provider

import (
	"context"
	"encoding/json"
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
	encoded, _ := json.Marshal(payload["input"])
	if strings.Contains(string(encoded), "private-uuid") {
		t.Fatal("provider received persistent book id")
	}
	if result.Value.ExistingBookRecommendations[0].BookID != "private-uuid" {
		t.Fatal("book alias was not restored")
	}
}
