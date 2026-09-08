package provider

import (
	"context"
	"encoding/json"
	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestConfiguredOrganizeMatchesPreviewAndRecordsActualModel(t *testing.T) {
	policy := promptconfig.Defaults("selected-lite", "selected-pro", "voice", 90)["journal"]
	temperature := 0.4
	policy.Journal.Organize.Lite.Temperature = &temperature
	policy.Journal.Organize.Lite.ReasoningEffort = "high"
	policy.Journal.Organize.Lite.MaxOutputTokens = 2048
	policy.Journal.Organize.Prompt = "可信的配置提示词"
	ctx := promptconfig.WithRevision(context.Background(), promptconfig.Revision{ID: "frozen", Scope: "journal", Policy: policy})
	request := contracts.OrganizeRequest{Title: "合成样例", Body: "周末去公园散步。"}
	prepared, _, err := PrepareOrganize(ctx, request, false, Config{})
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	_ = json.Unmarshal(prepared.Body, &want)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		if !reflect.DeepEqual(want, got) {
			t.Error("production and preview requests differ")
		}
		if got["instructions"] != policy.Journal.Organize.Prompt || got["model"] != "selected-lite" || got["temperature"] != temperature || got["max_output_tokens"] != float64(2048) {
			t.Error("parameters not applied")
		}
		_, _ = w.Write([]byte(`{"status":"completed","model":"actual-model","usage":{"input_tokens":20,"output_tokens":10},"output":[{"content":[{"type":"output_text","text":"{\"tags\":[],\"existingBookRecommendations\":[],\"newBookSuggestions\":[]}"}]}]}`))
	}))
	defer server.Close()
	result, err := New(Config{BaseURL: server.URL}, server.Client()).Organize(ctx, request, false)
	if err != nil || result.Model != "actual-model" || result.ConfigVersion != "frozen" {
		t.Fatal(result, err)
	}
}
