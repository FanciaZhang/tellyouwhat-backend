package birthday

import (
	"context"
	"encoding/json"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sampleRequest() Request {
	return Request{ContractVersion: 2, PromptVersion: 1, RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", SessionID: "b7a3d5fb-c14d-4b01-854e-232c8e89d066", Operation: "propose", CelebrationDate: "2026-12-20", TimeZone: "Asia/Shanghai", LeadTimeDays: 20, Currency: "CNY", Answers: map[string]string{}}
}
func sampleResult() ModelResult {
	return ModelResult{Kind: "proposals", Questions: []Question{}, Changes: []string{}, Proposals: []Proposal{{ID: "call", Title: "生日通话", Reason: "安排一次交流", Actions: []Action{{ID: "a1", Kind: "call", Title: "打电话", Detail: "按方便时间联系"}}, EvidenceIDs: []string{}, Preparations: []Preparation{}, BudgetNote: "预算未确定", Unknowns: []string{}, Tradeoff: "双方需要有空", Alternative: "文字祝福"}}}
}
func TestContractAllowsZeroPreparationsAndRejectsUnfoundedFacts(t *testing.T) {
	r, m := sampleRequest(), sampleResult()
	if r.Validate() != nil || m.Validate(r) != nil {
		t.Fatal("simple call should need no checklist")
	}
	m.Proposals[0].EvidenceIDs = []string{"invented"}
	if m.Validate(r) == nil {
		t.Fatal("invented evidence accepted")
	}
	m = sampleResult()
	amount := 201.0
	maximum := 200.0
	r.BudgetMaximum = &maximum
	m.Proposals[0].BudgetUpperBound = &amount
	if m.Validate(r) == nil {
		t.Fatal("over-budget proposal accepted")
	}
	m = sampleResult()
	m.Proposals[0].Preparations = []Preparation{{ID: "s1", ActionID: "a1", Title: "循环", Completion: "完成", DependsOn: []string{"s1"}}}
	if m.Validate(r) == nil {
		t.Fatal("cyclic steps accepted")
	}
}
func TestQuestionsCannotRepeatOrExceedTwo(t *testing.T) {
	r := sampleRequest()
	r.QuestionsAsked = 2
	m := ModelResult{Kind: "clarification", Questions: []Question{{ID: "delivery", Title: "怎样交付", Choices: []string{"当面", "寄送"}}}}
	if m.Validate(r) == nil {
		t.Fatal("third question accepted")
	}
	r.QuestionsAsked = 1
	r.Answers["delivery"] = "当面"
	if m.Validate(r) == nil {
		t.Fatal("answered question repeated")
	}
	r.Answers = map[string]string{}
	r.SkipQuestions = true
	if m.Validate(r) == nil {
		t.Fatal("skip ignored")
	}
}

func TestSchemaConstrainsKindsAndUnknownBudget(t *testing.T) {
	r := sampleRequest()
	r.SkipQuestions = true
	properties := ResponseSchema(r)["properties"].(map[string]any)
	kinds := properties["kind"].(map[string]any)["enum"].([]string)
	if len(kinds) != 1 || kinds[0] != "proposals" {
		t.Fatal("skip must constrain the model to proposals")
	}
	proposal := properties["proposals"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if proposal["budgetUpperBound"].(map[string]any)["type"] != "null" {
		t.Fatal("an unknown budget must not be generated as a number")
	}
	action := proposal["actions"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if !contains(action["kind"].(map[string]any)["enum"].([]string), "call") {
		t.Fatal("action kinds are missing from the model schema")
	}
	r.Evidence = []Evidence{{ID: "f1", Kind: "taboo", Text: "不喜欢聚餐"}}
	if contains(actionKinds(r), "meal") {
		t.Fatal("dining taboo is missing from model kinds")
	}
}
func TestDiningTabooAndAvoidedGiftAreRejected(t *testing.T) {
	r, m := sampleRequest(), sampleResult()
	r.Evidence = []Evidence{{ID: "f1", Kind: "taboo", Text: "不喜欢聚餐"}}
	m.Proposals[0].Actions[0].Kind = "meal"
	if m.Validate(r) == nil {
		t.Fatal("dining synonym accepted")
	}
	r = sampleRequest()
	m = sampleResult()
	r.Evidence = []Evidence{{ID: "h1", Kind: "history", Text: "去年送过杯子"}}
	r.History = []History{{SourceID: "h1", Title: "杯子", ReusePolicy: "avoidRepeating"}}
	m.Proposals[0].Actions[0].Kind = "gift"
	m.Proposals[0].Actions[0].Title = "新杯子"
	if m.Validate(r) == nil {
		t.Fatal("avoided gift accepted")
	}
}
func TestProviderUsesServerPromptAndValidatesActualHTTPOutput(t *testing.T) {
	r := sampleRequest()
	model := sampleResult()
	modelJSON, _ := json.Marshal(model)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		if req.URL.Path != "/responses" || req.Header.Get("Authorization") != "Bearer synthetic-test-key" {
			t.Error("provider route/auth")
		}
		var payload map[string]any
		_ = json.NewDecoder(req.Body).Decode(&payload)
		if payload["store"] != false || !strings.Contains(payload["instructions"].(string), "累计最多两问") {
			t.Error("server prompt/store policy missing")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(modelJSON)}}}}, "usage": map[string]int{"input_tokens": 12, "output_tokens": 25}})
	}))
	defer upstream.Close()
	client := New(Config{BaseURL: upstream.URL, APIKey: "synthetic-test-key", Model: "model-for-test"}, upstream.Client(), nil, costcontrol.TokenPrice{})
	result, err := client.Assist(context.Background(), r)
	if err != nil || result.Value.Proposals[0].Title != "生日通话" || result.OutputTokens != 25 || calls != 1 {
		t.Fatalf("provider roundtrip: %v", err)
	}
}
