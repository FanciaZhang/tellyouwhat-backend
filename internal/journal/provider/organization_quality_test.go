package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func TestOrganizationFiltersLowValueAndUnsupportedTags(t *testing.T) {
	request := contracts.OrganizeRequest{
		Body: "和小林在北京散步，又聊起了 Road Trip。", ExistingTags: []string{"Road Trip"}, RejectedTagNames: []string{"散步"},
	}
	result := contracts.ModelResult{Tags: []contracts.Tag{
		{Name: "生活", Type: "topic"}, {Name: "ＲＯＡＤ　ＴＲＩＰ", Type: "topic"},
		{Name: "散步", Type: "event"}, {Name: "北京", Type: "place"},
		{Name: "小林", Type: "person"}, {Name: "上海", Type: "place"},
		{Name: "老婆", Type: "person"}, {Name: "#出游", Type: "topic"},
	}}
	curated := curateOrganization(request, result)
	want := []contracts.Tag{{Name: "北京", Type: "place"}, {Name: "小林", Type: "person"}}
	if !reflect.DeepEqual(curated.Tags, want) {
		t.Fatalf("low-value, rejected, manual or invented tags survived: %+v", curated.Tags)
	}
	if request.ExistingTags[0] != "Road Trip" || result.Tags[1].Name != "ＲＯＡＤ　ＴＲＩＰ" {
		t.Fatal("curation mutated the user's names or provider evidence")
	}
}

func TestOrganizationDeduplicatesWidthVariantsAndLimitsTags(t *testing.T) {
	result := contracts.ModelResult{Tags: []contracts.Tag{
		{Name: "AI", Type: "topic"}, {Name: "ＡＩ", Type: "topic"},
		{Name: "阅读", Type: "topic"}, {Name: "工作复盘", Type: "topic"},
		{Name: "写作", Type: "topic"}, {Name: "育儿", Type: "topic"},
		{Name: "旅行", Type: "topic"}, {Name: "绘画", Type: "topic"},
	}}
	curated := curateOrganization(contracts.OrganizeRequest{}, result)
	if len(curated.Tags) != 5 || curated.Tags[1].Name != "阅读" || curated.Tags[4].Name != "育儿" {
		t.Fatalf("expected five distinct leading tags: %+v", curated.Tags)
	}
}

func TestOrganizationBookNamesAndRelatedTags(t *testing.T) {
	request := contracts.OrganizeRequest{ExistingTags: []string{"情侣出游"}, RejectedTagNames: []string{"旅游"}}
	result := contracts.ModelResult{
		Tags: []contracts.Tag{{Name: "徒步", Type: "topic"}, {Name: "生活", Type: "topic"}},
		NewBookSuggestions: []contracts.NewBookSuggestion{
			{Name: "情侣出游手记", Reason: "写到了两人一起徒步。", RelatedTags: []string{"情侣出游", "徒步", "生活", "旅游", "海边"}},
			{Name: "情侣出游", Reason: "同一个主题。", RelatedTags: []string{}},
		},
	}
	curated := curateOrganization(request, result)
	if len(curated.NewBookSuggestions) != 1 {
		t.Fatalf("redundant book proposals survived: %+v", curated.NewBookSuggestions)
	}
	suggestion := curated.NewBookSuggestions[0]
	if suggestion.Name != "情侣出游" || !reflect.DeepEqual(suggestion.RelatedTags, []string{"情侣出游", "徒步"}) {
		t.Fatalf("book suffix or unrelated/rejected tags survived: %+v", suggestion)
	}
	if result.NewBookSuggestions[0].Name != "情侣出游手记" || len(result.NewBookSuggestions[0].RelatedTags) != 5 {
		t.Fatal("curation mutated provider evidence")
	}
}

func TestOrganizationReusesBooksAndPreservesUserTitles(t *testing.T) {
	for _, test := range []struct {
		name  string
		books []contracts.BookContext
		want  string
	}{
		{"existing suffix", []contracts.BookContext{{ID: "one", Name: "情侣出游手记"}}, "one"},
		{"exact title wins", []contracts.BookContext{{ID: "one", Name: "情侣出游手记"}, {ID: "two", Name: "情侣出游"}}, "two"},
		{"already joined", []contracts.BookContext{{ID: "one", Name: "情侣出游手记", ContainsEntry: true}}, ""},
		{"ambiguous originals", []contracts.BookContext{{ID: "one", Name: "情侣出游手记"}, {ID: "two", Name: "情侣出游手記"}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := contracts.OrganizeRequest{Books: test.books}
			before, _ := json.Marshal(request)
			result := contracts.ModelResult{NewBookSuggestions: []contracts.NewBookSuggestion{{Name: "情侣出游手记", Reason: "两人一起出游。"}}}
			curated := curateOrganization(request, result)
			if len(curated.NewBookSuggestions) != 0 {
				t.Fatal("suggestion would duplicate a user's existing book")
			}
			if test.want == "" {
				if len(curated.ExistingBookRecommendations) != 0 {
					t.Fatal("already joined or ambiguous book was recommended")
				}
			} else if len(curated.ExistingBookRecommendations) != 1 || curated.ExistingBookRecommendations[0].BookID != test.want {
				t.Fatalf("wrong existing book: %+v", curated.ExistingBookRecommendations)
			}
			after, _ := json.Marshal(request)
			if string(before) != string(after) {
				t.Fatal("existing user book was renamed")
			}
		})
	}
}

func TestOrganizationOnlyRemovesRedundantJournalSuffix(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"日常思考复盘手记", "日常思考复盘"}, {"情侣出游手記冊", "情侣出游"},
		{"情侣出游 · 手记", "情侣出游"}, {"手记", ""}, {"手记与摄影", ""},
		{"两人出游记录", "两人出游"},
		{"旅行", "旅行"}, {strings.Repeat("长", 25), ""},
	} {
		if got := conciseBookName(test.input); got != test.want {
			t.Fatalf("%q: got %q, want %q", test.input, got, test.want)
		}
	}
}

func TestOrganizationAvoidsRepeatingTopicsAndRejectedSynonyms(t *testing.T) {
	request := contracts.OrganizeRequest{ExistingTags: []string{"旅行"}, RejectedTagNames: []string{"工作复盘"}}
	result := contracts.ModelResult{Tags: []contracts.Tag{
		{Name: "情侣出游", Type: "topic"}, {Name: "旅游", Type: "topic"},
		{Name: "每周复盘", Type: "event"}, {Name: "任务安排", Type: "topic"},
		{Name: "育儿", Type: "topic"},
	}}
	curated := curateOrganization(request, result)
	if !reflect.DeepEqual(curated.Tags, []contracts.Tag{{Name: "任务安排", Type: "topic"}, {Name: "育儿", Type: "topic"}}) {
		t.Fatalf("manual/rejected topic variants survived: %+v", curated.Tags)
	}
	curated = curateOrganization(contracts.OrganizeRequest{}, result)
	if len(curated.Tags) != 4 || curated.Tags[0].Name != "情侣出游" {
		t.Fatalf("topic synonyms were not collapsed: %+v", curated.Tags)
	}
}

func TestOrganizationDoesNotMergeUnrelatedSuffixesAndMatchesAppNames(t *testing.T) {
	result := contracts.ModelResult{Tags: []contracts.Tag{
		{Name: "旅行", Type: "topic"}, {Name: "时间旅行", Type: "topic"},
		{Name: "工作复盘", Type: "topic"}, {Name: "游戏复盘", Type: "topic"},
		{Name: "Café", Type: "topic"}, {Name: "Cafe", Type: "topic"},
	}}
	curated := curateOrganization(contracts.OrganizeRequest{}, result)
	if len(curated.Tags) != 5 || curated.Tags[1].Name != "时间旅行" || curated.Tags[3].Name != "游戏复盘" {
		t.Fatalf("distinct topics were lost or accent variants were duplicated: %+v", curated.Tags)
	}
	request := contracts.OrganizeRequest{ExistingTags: []string{"ＣＡＦＥ"}, Books: []contracts.BookContext{{ID: "one", Name: "Café手记"}}}
	result.NewBookSuggestions = []contracts.NewBookSuggestion{{Name: "Cafe", Reason: "咖啡馆见闻。"}}
	curated = curateOrganization(request, result)
	if len(curated.Tags) != 4 || len(curated.NewBookSuggestions) != 0 || len(curated.ExistingBookRecommendations) != 1 {
		t.Fatalf("normalization differs from the App: %+v", curated)
	}
}

func TestProviderReturnsCuratedCompatibleOutputAndKeepsUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload["instructions"] != promptconfig.OrganizePrompt {
			t.Error("provider did not receive the quality instructions")
		}
		schema := payload["text"].(map[string]any)["format"].(map[string]any)["schema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		if properties["tags"].(map[string]any)["maxItems"] != float64(5) || properties["newBookSuggestions"].(map[string]any)["maxItems"] != float64(1) {
			t.Error("generation limits were not tightened")
		}
		output, _ := json.Marshal(contracts.ModelResult{
			Tags:                        []contracts.Tag{{Name: "生活", Type: "topic"}, {Name: "旅行", Type: "topic"}},
			ExistingBookRecommendations: []contracts.ExistingBookRecommendation{{BookID: "b1", Reason: "已经收录。"}},
			NewBookSuggestions:          []contracts.NewBookSuggestion{{Name: "旅行手记", Reason: "写到一起旅行。", RelatedTags: []string{"旅行"}}},
		})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed", "usage": map[string]int{"input_tokens": 123, "output_tokens": 45},
			"output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(output)}}}},
		})
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL, LiteModel: "lite"}, server.Client())
	result, err := client.Organize(context.Background(), contracts.OrganizeRequest{
		Body: "一起旅行。", Books: []contracts.BookContext{{ID: "already-joined", Name: "已有书册", ContainsEntry: true}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.InputTokens != 123 || result.OutputTokens != 45 || len(result.Value.Tags) != 1 || len(result.Value.ExistingBookRecommendations) != 0 || result.Value.NewBookSuggestions[0].Name != "旅行" {
		t.Fatalf("curation or metering failed: %+v", result)
	}
	if err := result.Value.Validate(map[string]bool{"already-joined": true}); err != nil {
		t.Fatalf("curated output no longer fits the App's existing contract: %v", err)
	}
	encoded, _ := json.Marshal(curateOrganization(contracts.OrganizeRequest{}, contracts.ModelResult{}))
	if strings.Contains(string(encoded), "\"tags\":null") || strings.Contains(string(encoded), "\"existingBookRecommendations\":null") || strings.Contains(string(encoded), "\"newBookSuggestions\":null") {
		t.Fatalf("empty results must remain JSON arrays for the App decoder: %s", encoded)
	}
}
