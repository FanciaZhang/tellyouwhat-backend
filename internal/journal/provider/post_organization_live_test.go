package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
)

func TestLivePostOrganizationWithSyntheticManuscripts(t *testing.T) {
	path := os.Getenv("JOURNAL_POST_ORGANIZATION_CONFIG")
	if path == "" {
		t.Skip("explicit configured-provider acceptance only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config Config
	if err = json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if config.APIKey == "" || config.LiteModel == "" {
		t.Fatal("configured test model unavailable")
	}
	client := New(config, &http.Client{Timeout: 75 * time.Second})
	inputs := []string{
		"周五下班很晚，我给爸爸打电话，说不用等我。他还是在车站出口等着，穿着那件灰色旧外套。回家的路上他没问工作，只说汤还热着。我忽然发现，他已经走得比以前慢了。",
		"周末整理了书房。把旧纸箱拆开，把三层书架重新擦了一遍，找到去年没读完的小说。下午下了一阵雨，我坐在窗边读完最后一页，杯里的茶还温着。",
		"我总以为每件事都要立刻想明白。今天散步时，走到河岸边，忽然不想追着一个答案跑了。问题还在那里，心里倒比早上安静了一点。",
		"I spent Sunday sorting the books in my study. I found the novel I had abandoned last winter. Rain tapped against the window while I finally finished its last chapter. My tea was still warm.",
	}
	var results []map[string]any
	for i, body := range inputs {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		started := time.Now()
		result, err := client.Organize(ctx, contracts.OrganizeRequest{PostOrganization: true, GenerateTitle: true, IllustrationSuggestionsEnabled: true, Body: body}, os.Getenv("JOURNAL_POST_ORGANIZATION_PRO") == "1")
		cancel()
		if err != nil {
			t.Fatalf("synthetic case %d: %v", i, err)
		}
		if result.Value.SuggestedTitle == nil || !contracts.ValidEntryTitle(*result.Value.SuggestedTitle) {
			t.Fatalf("synthetic case %d has no valid title", i)
		}
		if i == 3 && strings.ContainsFunc(*result.Value.SuggestedTitle, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
			t.Fatal("English manuscript title was translated")
		}
		results = append(results, map[string]any{"body": body, "result": result.Value, "model": result.Model, "seconds": time.Since(started).Seconds(), "inputTokens": result.InputTokens, "outputTokens": result.OutputTokens})
	}
	output := os.Getenv("JOURNAL_POST_ORGANIZATION_OUTPUT")
	if output == "" {
		t.Fatal("acceptance output directory required")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(filepath.Join(output, "synthetic-post-organization.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("four synthetic manuscripts each returned title and illustration advice through one model request")
}
