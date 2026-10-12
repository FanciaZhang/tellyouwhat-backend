package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

// Opt-in acceptance against the configured real Lite and Pro models. These
// deliberately synthetic journals never come from a user's saved entries.
func TestLiveJournalOrganizationQuality(t *testing.T) {
	if os.Getenv("JOURNAL_ORGANIZE_LIVE_CHECK") != "1" {
		t.Skip("explicit live provider acceptance only")
	}
	base := os.Getenv("JOURNAL_ARK_BASE_URL")
	if base == "" {
		base = "https://ark.cn-beijing.volces.com/api/v3"
	}
	var runtime *promptconfig.Revision
	if path := os.Getenv("JOURNAL_ORGANIZE_POLICY_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var revision promptconfig.Revision
		if err := json.Unmarshal(data, &revision); err != nil {
			t.Fatal(err)
		}
		if err := revision.Policy.Validate("journal"); err != nil {
			t.Fatal(err)
		}
		// Retain the real published model/parameter settings, testing only the proposed Prompt.
		revision.Policy.Journal.Organize.Prompt = promptconfig.OrganizePrompt
		runtime = &revision
	}
	bookID := "00000000-0000-4000-8000-000000000001"
	reflection := "连续三周，我都在周末复盘自己的工作与生活。今天回顾了任务安排，发现我经常把简单的事拖到最后。我想以后每周都留一段时间思考自己的选择，并记录下一次可以怎样改进。"
	travel := "我和女朋友小林约好，以后每个月一起去一个地方旅行。这次去了桂林的阳朔，在漓江边骑行。以后还想一起去别的城市，慢慢记下两个人出游的经历。"
	for _, pro := range []bool{false, true} {
		model := "lite"
		if pro {
			model = "pro"
		}
		for _, test := range []struct {
			name, title, body  string
			existing, rejected []string
			books              []contracts.BookContext
			wantNew            bool
			wantExisting       bool
			sparse             bool
		}{
			{name: "reflection", title: "每周复盘", body: reflection, wantNew: true},
			{name: "couple-travel", title: "一起出游", body: travel, wantNew: true},
			{name: "sparse", body: "今天吃了碗面，挺好吃。", sparse: true},
			{name: "reuse-user-title", title: "每周复盘", body: reflection, wantExisting: true,
				books: []contracts.BookContext{{ID: bookID, Name: "日常思考复盘手记", Description: "长期收录工作、生活中的思考与复盘。"}}},
			{name: "already-collected", title: "一起出游", body: travel, existing: []string{"旅行"}, rejected: []string{"骑行"},
				books: []contracts.BookContext{{ID: bookID, Name: "情侣出游手记", Description: "我们两个人的出游经历。", ContainsEntry: true}}},
		} {
			t.Run(model+"/"+test.name, func(t *testing.T) {
				request := contracts.OrganizeRequest{RequestID: uuid.NewString(), ContractVersion: contracts.ContractVersion,
					ContentHash: strings.Repeat("0", 64), Title: test.title, Body: test.body,
					ExistingTags: test.existing, RejectedTagNames: test.rejected, Books: test.books}
				if request.ExistingTags == nil {
					request.ExistingTags = []string{}
				}
				if request.RejectedTagNames == nil {
					request.RejectedTagNames = []string{}
				}
				if request.Books == nil {
					request.Books = []contracts.BookContext{}
				}
				capture := &organizationResponseCapture{next: http.DefaultTransport}
				client := New(Config{BaseURL: base, APIKey: os.Getenv("JOURNAL_ARK_API_KEY"),
					LiteModel: os.Getenv("JOURNAL_ARK_LITE_MODEL_ID"), ProModel: os.Getenv("JOURNAL_ARK_PRO_MODEL_ID")},
					&http.Client{Timeout: 75 * time.Second, Transport: capture})
				ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
				defer cancel()
				if runtime != nil {
					ctx = promptconfig.WithRevision(ctx, *runtime)
				}
				result, err := client.Organize(ctx, request, pro)
				raw, rawErr := capture.modelResult()
				writeOrganizationEvidence(t, model+"-"+test.name, request, raw, result, err)
				if err != nil {
					t.Fatal(err)
				}
				if rawErr != nil {
					t.Fatal(rawErr)
				}
				if result.InputTokens <= 0 || result.OutputTokens <= 0 {
					t.Fatal("missing real provider usage")
				}
				// Check unfiltered output as well: a passing guard cannot conceal a weak Prompt.
				if len(raw.Tags) > 5 || len(raw.NewBookSuggestions) > 1 {
					t.Error("model padded the result")
				}
				seen := map[string]bool{}
				families := map[string]bool{}
				for _, tag := range raw.Tags {
					key := qualityName(tag.Name)
					if emptyOrganizationTags[key] || seen[key] || strings.HasPrefix(key, "#") {
						t.Errorf("low-value or duplicate tag: %q", tag.Name)
					}
					seen[key] = true
					if family := organizationTagFamily(key); family != "" {
						if families[family] {
							t.Errorf("near-synonymous tags in family %q", family)
						}
						families[family] = true
					}
					if (tag.Type == "person" || tag.Type == "place" || tag.Type == "organization") && !strings.Contains(qualityName(test.title+"\n"+test.body), key) {
						t.Errorf("named entity was invented: %q", tag.Name)
					}
					for _, excluded := range append(append([]string{}, test.existing...), test.rejected...) {
						if key == qualityName(excluded) {
							t.Errorf("manual or rejected tag returned: %q", tag.Name)
						}
					}
				}
				for _, book := range raw.NewBookSuggestions {
					if strings.Contains(book.Name, "手记") || strings.Contains(book.Name, "手記") {
						t.Errorf("redundant generated book title: %q", book.Name)
					}
					if strings.HasSuffix(book.Name, "记录") || strings.HasSuffix(book.Name, "合集") {
						t.Errorf("redundant generated suffix: %q", book.Name)
					}
				}
				if test.sparse && len(raw.Tags) > 1 {
					t.Error("sparse content generated excessive tags")
				}
				if test.wantNew {
					if len(result.Value.NewBookSuggestions) != 1 {
						t.Error("explicit recurring topic should offer a concise new book")
					}
				} else if len(raw.NewBookSuggestions) != 0 {
					t.Error("unnecessary new book despite sparse content or an existing match")
				}
				if test.wantExisting {
					if len(result.Value.ExistingBookRecommendations) != 1 || result.Value.ExistingBookRecommendations[0].BookID != bookID {
						t.Error("matching user-named book was not reused")
					}
				} else if len(result.Value.ExistingBookRecommendations) != 0 {
					t.Error("already collected book was recommended")
				}
				t.Logf("real %s: tags=%v, new=%v, existing=%v, tokens=%d/%d", model, result.Value.Tags,
					result.Value.NewBookSuggestions, result.Value.ExistingBookRecommendations, result.InputTokens, result.OutputTokens)
			})
		}
	}
}

type organizationResponseCapture struct {
	next http.RoundTripper
	body []byte
}

func (c *organizationResponseCapture) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := c.next.RoundTrip(request)
	if err != nil {
		return response, err
	}
	c.body, err = io.ReadAll(io.LimitReader(response.Body, contracts.MaxBodyBytes+1))
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(c.body))
	return response, err
}

func (c *organizationResponseCapture) modelResult() (contracts.ModelResult, error) {
	var envelope struct {
		Output []struct{ Content []struct{ Type, Text string } }
	}
	if err := json.Unmarshal(c.body, &envelope); err != nil {
		return contracts.ModelResult{}, err
	}
	var text string
	for _, output := range envelope.Output {
		for _, content := range output.Content {
			if content.Type == "output_text" {
				text += content.Text
			}
		}
	}
	var raw contracts.ModelResult
	err := json.Unmarshal([]byte(text), &raw)
	return raw, err
}

func writeOrganizationEvidence(t *testing.T, name string, request contracts.OrganizeRequest, raw contracts.ModelResult, result Result, resultErr error) {
	t.Helper()
	root := os.Getenv("JOURNAL_ORGANIZE_EVIDENCE_DIR")
	if root == "" {
		return
	}
	if !filepath.IsAbs(root) {
		t.Fatal("evidence directory must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	errorText := ""
	if resultErr != nil {
		errorText = resultErr.Error()
	}
	evidence := map[string]any{"analysisVersion": OrganizationAnalysisVersion, "request": request,
		"rawModelResult": raw, "rawOutputText": result.OutputText, "curatedResult": result.Value, "basePublishedPromptVersion": result.ConfigVersion,
		"promptSource":  "local-candidate",
		"providerModel": result.Model, "inputTokens": result.InputTokens, "outputTokens": result.OutputTokens, "error": errorText}
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if resultErr != nil {
		t.Logf("failed real attempt preserved: %s", name)
	}
}
