package birthday

import (
	"encoding/json"
	"testing"
)

func sampleBlueprint() *Blueprint {
	return &Blueprint{Scene: "gallery", Premise: "一件旧物，一个故事", PreparationMinutes: 20, SceneItems: []string{"旧物", "展签"},
		Moments: []Moment{{Title: "讲一个故事", Detail: "选择一件真实发生的小事", Minutes: 15}}, Materials: []Material{{Name: "卡纸", Detail: "可用现有纸张"}},
		Keepsake: Keepsake{Title: "有些小事，我记得", Body: "邀请你来看看", Footer: "时间与地点待约定"}}
}
func TestBlueprintBudgetEffortAndPersistence(t *testing.T) {
	r := sampleRequest()
	r.PromptVersion = 2
	r.PlanDetail = "full"
	r.Operation = "revise"
	r.Effort = "simple"
	m := sampleResult()
	r.CurrentProposal = &m.Proposals[0]
	m.Kind = "revision"
	if m.Validate(r) == nil {
		t.Fatal("full expansion without blueprint accepted")
	}
	m.Proposals[0].Blueprint = sampleBlueprint()
	if r.Validate() != nil || m.Validate(r) != nil {
		t.Fatal("valid complete plan rejected")
	}
	raw, _ := json.Marshal(m)
	var restored ModelResult
	if json.Unmarshal(raw, &restored) != nil || restored.Validate(r) != nil {
		t.Fatal("full plan did not survive serialization")
	}
	amount, ceiling := 101.0, 100.0
	r.BudgetMaximum = &ceiling
	m.Proposals[0].BudgetUpperBound = &ceiling
	m.Proposals[0].Blueprint.Materials[0].Amount = &amount
	if m.Validate(r) == nil {
		t.Fatal("material sum over budget accepted")
	}
	amount = 30
	m.Proposals[0].Blueprint.Materials = append(m.Proposals[0].Blueprint.Materials, Material{Name: "纸张", Detail: "少量", Amount: &amount})
	ceiling = 50
	if m.Validate(r) == nil {
		t.Fatal("individual prices pass but total exceeds ceiling")
	}
	ceiling = 100
	if m.Validate(r) != nil {
		t.Fatal("valid material sum rejected")
	}
	r.BudgetMaximum = nil
	if m.Validate(r) == nil {
		t.Fatal("unknown budget with numeric material costs accepted")
	}
	r.BudgetMaximum = &ceiling
	m.Proposals[0].Blueprint.PreparationMinutes = 31
	if m.Validate(r) == nil {
		t.Fatal("simple plan exceeds preparation limit")
	}
	m.Proposals[0].Blueprint.PreparationMinutes = 20
	m.Proposals[0].Blueprint.Moments[0].Minutes = 46
	if m.Validate(r) == nil {
		t.Fatal("simple plan takes too long")
	}
	m.Proposals[0].Blueprint.Moments[0].Minutes = 15
	m.Proposals[0].Blueprint.Keepsake.Body = "保证送达"
	if m.Validate(r) == nil {
		t.Fatal("unsupported promise in card accepted")
	}
}
func TestCreativeInputsAndSchemas(t *testing.T) {
	r := sampleRequest()
	r.PromptVersion = 2
	r.PlanDetail = "ideas"
	props := ResponseSchema(r)["properties"].(map[string]any)["proposals"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if props["blueprint"].(map[string]any)["type"] != "null" {
		t.Fatal("ideas should not generate full documents")
	}
	r.Effort = "unlimited"
	if r.Validate() == nil {
		t.Fatal("unsupported effort accepted")
	}
	r.Effort = "thoughtful"
	r.PlanDetail = "full"
	if r.Validate() == nil {
		t.Fatal("expansion needs an existing idea")
	}
}

func TestNegativeTabooRestatementAndEmptyCollections(t *testing.T) {
	for _, text := range []string{"不公开庆祝", "两个人在家，不安排公开庆祝", "避免公开庆祝"} {
		if containsAffirmative(text, "公开庆祝") {
			t.Fatal("negative restatement rejected")
		}
	}
	for _, text := range []string{"安排公开庆祝", "不只公开庆祝", "不公开庆祝，但准备一场公开庆祝"} {
		if !containsAffirmative(text, "公开庆祝") {
			t.Fatal("positive occurrence escaped validation")
		}
	}
	m := sampleResult()
	m.Changes = nil
	m.Proposals[0].Unknowns = nil
	m.normalizeEmptyCollections()
	b, _ := json.Marshal(m)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if raw["changes"] == nil {
		t.Fatal("null collection leaked into Swift response")
	}
}
