// Package birthday owns the scoped, versioned birthday decision contract.
package birthday

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

const MaxBodyBytes = 64 << 10
const OutputTokens = 6000

var ErrInvalid = errors.New("invalid birthday contract")

type Evidence struct {
	ID   string  `json:"id"`
	Kind string  `json:"kind"`
	Text string  `json:"text"`
	Date *string `json:"date"`
}
type History struct {
	SourceID          string   `json:"sourceID"`
	Title             string   `json:"title"`
	Category          string   `json:"category"`
	ActualCelebration string   `json:"actualCelebration"`
	Reaction          string   `json:"reaction"`
	Tags              []string `json:"tags"`
	ReusePolicy       string   `json:"reusePolicy"`
	NextTimeNote      string   `json:"nextTimeNote"`
	Date              string   `json:"date"`
}
type Action struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}
type Preparation struct {
	ID            string   `json:"id"`
	ActionID      string   `json:"actionID"`
	Title         string   `json:"title"`
	Completion    string   `json:"completion"`
	DependsOn     []string `json:"dependsOn"`
	EstimatedDays *int     `json:"estimatedDays"`
	TimingBasis   *string  `json:"timingBasis"`
}
type Moment struct {
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	Minutes int    `json:"minutes"`
}
type Material struct {
	Name   string   `json:"name"`
	Detail string   `json:"detail"`
	Amount *float64 `json:"amount"`
}
type Keepsake struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Footer string `json:"footer"`
}
type Blueprint struct {
	Scene              string     `json:"scene"`
	Premise            string     `json:"premise"`
	PreparationMinutes int        `json:"preparationMinutes"`
	SceneItems         []string   `json:"sceneItems"`
	Moments            []Moment   `json:"moments"`
	Materials          []Material `json:"materials"`
	Keepsake           Keepsake   `json:"keepsake"`
}
type Proposal struct {
	Blueprint        *Blueprint    `json:"blueprint"`
	ID               string        `json:"id"`
	Title            string        `json:"title"`
	Reason           string        `json:"reason"`
	EvidenceIDs      []string      `json:"evidenceIDs"`
	Actions          []Action      `json:"actions"`
	Preparations     []Preparation `json:"preparations"`
	BudgetUpperBound *float64      `json:"budgetUpperBound"`
	BudgetNote       string        `json:"budgetNote"`
	Unknowns         []string      `json:"unknowns"`
	Tradeoff         string        `json:"tradeoff"`
	Alternative      string        `json:"alternative"`
}
type Question struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Choices []string `json:"choices"`
}
type Request struct {
	PlanDetail            string            `json:"planDetail,omitempty"`
	Effort                string            `json:"effort,omitempty"`
	ContractVersion       int               `json:"contractVersion"`
	PromptVersion         int               `json:"promptVersion"`
	RequestID             string            `json:"requestID"`
	SessionID             string            `json:"sessionID"`
	DraftRevision         int               `json:"draftRevision"`
	Operation             string            `json:"operation"`
	CelebrationDate       string            `json:"celebrationDate"`
	TimeZone              string            `json:"timeZone"`
	LeadTimeDays          int               `json:"leadTimeDays"`
	RelationshipTags      []string          `json:"relationshipTags"`
	Idea                  string            `json:"idea"`
	BudgetText            string            `json:"budgetText"`
	BudgetMaximum         *float64          `json:"budgetMaximum"`
	Currency              string            `json:"currency"`
	Evidence              []Evidence        `json:"evidence"`
	History               []History         `json:"history"`
	Answers               map[string]string `json:"answers"`
	QuestionsAsked        int               `json:"questionsAsked"`
	SkipQuestions         bool              `json:"skipQuestions"`
	CurrentProposal       *Proposal         `json:"currentProposal"`
	CompletedPreparations []string          `json:"completedPreparations"`
	Change                string            `json:"change"`
}
type ModelResult struct {
	Kind      string     `json:"kind"`
	Questions []Question `json:"questions"`
	Proposals []Proposal `json:"proposals"`
	Changes   []string   `json:"changes"`
	Notice    string     `json:"notice"`
}
type Response struct {
	ContractVersion int    `json:"contractVersion"`
	RequestID       string `json:"requestID"`
	DraftRevision   int    `json:"draftRevision"`
	Source          string `json:"source"`
	ModelResult
}

func (r Request) Validate() error {
	if r.PromptVersion != 2 && (r.PlanDetail != "" || r.Effort != "") {
		return ErrInvalid
	}
	if !contains([]string{"", "ideas", "full"}, r.PlanDetail) || !contains([]string{"", "simple", "thoughtful"}, r.Effort) {
		return ErrInvalid
	}
	if r.PlanDetail == "full" && (r.Operation != "revise" || r.CurrentProposal == nil) {
		return ErrInvalid
	}
	if r.ContractVersion != 2 || (r.PromptVersion != 1 && r.PromptVersion != 2) || r.DraftRevision < 0 || r.DraftRevision > 10000 ||
		(r.Operation != "propose" && r.Operation != "revise") || r.Currency != "CNY" || r.QuestionsAsked < 0 || r.QuestionsAsked > 2 ||
		r.LeadTimeDays < 0 || r.LeadTimeDays > 36525 || len(r.Evidence) > 28 || len(r.History) > 8 || len(r.Answers) > 2 {
		return ErrInvalid
	}
	if _, err := uuid.Parse(r.RequestID); err != nil {
		return ErrInvalid
	}
	if _, err := uuid.Parse(r.SessionID); err != nil {
		return ErrInvalid
	}
	if _, err := time.Parse("2006-01-02", r.CelebrationDate); err != nil {
		return ErrInvalid
	}
	if _, err := time.LoadLocation(r.TimeZone); err != nil {
		return ErrInvalid
	}
	if r.Operation == "revise" && r.CurrentProposal == nil {
		return ErrInvalid
	}
	if r.BudgetMaximum != nil && (math.IsNaN(*r.BudgetMaximum) || math.IsInf(*r.BudgetMaximum, 0) || *r.BudgetMaximum < 0) {
		return ErrInvalid
	}
	sources := map[string]string{}
	for _, e := range r.Evidence {
		if e.ID == "" || e.Text == "" || sources[e.ID] != "" || (e.Kind != "preference" && e.Kind != "taboo" && e.Kind != "history") {
			return ErrInvalid
		}
		sources[e.ID] = e.Kind
	}
	for _, h := range r.History {
		if sources[h.SourceID] != "history" || (h.ReusePolicy != "undecided" && h.ReusePolicy != "similarIsFine" && h.ReusePolicy != "avoidRepeating") {
			return ErrInvalid
		}
	}
	for key, value := range r.Answers {
		if key == "" || value == "" || len(value) > 500 {
			return ErrInvalid
		}
	}
	if len([]rune(r.Idea)) > 3000 || len([]rune(r.Change)) > 3000 || len(r.BudgetText) > 300 {
		return ErrInvalid
	}
	return nil
}
func (m ModelResult) Validate(r Request) error {
	if m.Kind == "clarification" {
		if r.SkipQuestions || r.Operation == "revise" || r.QuestionsAsked+len(m.Questions) > 2 || len(m.Questions) < 1 || len(m.Proposals) > 0 {
			return ErrInvalid
		}
		seen := map[string]bool{}
		for _, q := range m.Questions {
			if q.ID == "" || q.Title == "" || seen[q.ID] || r.Answers[q.ID] != "" || len(q.Choices) < 2 || len(q.Choices) > 5 {
				return ErrInvalid
			}
			seen[q.ID] = true
			for _, c := range q.Choices {
				if c == "" {
					return ErrInvalid
				}
			}
		}
		return nil
	}
	if len(m.Questions) > 0 || len(m.Proposals) < 1 || len(m.Proposals) > 3 || (r.Operation == "propose" && m.Kind != "proposals") || (r.Operation == "revise" && m.Kind != "revision") {
		return ErrInvalid
	}
	sources := map[string]bool{}
	for _, e := range r.Evidence {
		sources[e.ID] = true
	}
	proposals := map[string]bool{}
	for _, p := range m.Proposals {
		if p.ID == "" || proposals[p.ID] || p.Title == "" || p.Reason == "" || p.Tradeoff == "" || p.Alternative == "" || p.BudgetNote == "" || len(p.Actions) < 1 || len(p.Actions) > 5 || len(p.Preparations) > 5 {
			return ErrInvalid
		}
		proposals[p.ID] = true
		if r.PlanDetail == "full" && (p.Blueprint == nil || len(m.Proposals) != 1) {
			return ErrInvalid
		}
		if p.Blueprint != nil && p.Blueprint.Validate(r, p.BudgetUpperBound) != nil {
			return ErrInvalid
		}
		for _, ref := range p.EvidenceIDs {
			if !sources[ref] {
				return ErrInvalid
			}
		}
		if p.BudgetUpperBound != nil && (r.BudgetMaximum == nil || math.IsNaN(*p.BudgetUpperBound) || math.IsInf(*p.BudgetUpperBound, 0) || *p.BudgetUpperBound < 0 || *p.BudgetUpperBound > *r.BudgetMaximum) {
			return ErrInvalid
		}
		actions := map[string]bool{}
		actionText := p.Title
		if p.Blueprint != nil {
			b, _ := json.Marshal(p.Blueprint)
			actionText += string(b)
		}
		for _, a := range p.Actions {
			if a.ID == "" || actions[a.ID] || a.Title == "" || a.Detail == "" || !contains(actionKinds(r), a.Kind) {
				return ErrInvalid
			}
			actions[a.ID] = true
			actionText += " " + a.Title + a.Detail
		}
		steps := map[string]bool{}
		for _, s := range p.Preparations {
			if s.ID == "" || steps[s.ID] || !actions[s.ActionID] || s.Title == "" || s.Completion == "" {
				return ErrInvalid
			}
			for _, dependency := range s.DependsOn {
				if !steps[dependency] {
					return ErrInvalid
				}
			}
			if s.EstimatedDays != nil && (*s.EstimatedDays < 0 || *s.EstimatedDays > r.LeadTimeDays || s.TimingBasis == nil || *s.TimingBasis == "") {
				return ErrInvalid
			}
			steps[s.ID] = true
		}
		for _, e := range r.Evidence {
			if e.Kind != "taboo" {
				continue
			}
			if containsAffirmative(actionText, e.Text) {
				return ErrInvalid
			}
		}
		for _, h := range r.History {
			if h.ReusePolicy == "avoidRepeating" && len([]rune(h.Title)) >= 2 {
				for _, a := range p.Actions {
					if a.Kind == "gift" && strings.Contains(a.Title, h.Title) {
						return ErrInvalid
					}
				}
			}
		}
		for _, claim := range []string{"保证送达", "保证有货", "一定送到", "实时价格", "当天必达"} {
			if strings.Contains(actionText, claim) {
				return ErrInvalid
			}
		}
	}
	return nil
}
func actionKinds(r Request) []string {
	for _, e := range r.Evidence {
		if e.Kind == "taboo" && (strings.Contains(e.Text, "聚餐") || strings.Contains(e.Text, "吃饭") || strings.Contains(e.Text, "餐厅")) {
			return []string{"gift", "experience", "call", "visit", "custom", "skip"}
		}
	}
	return []string{"gift", "meal", "experience", "call", "visit", "custom", "skip"}
}
func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
func ReservationTokens(r Request) int { b, _ := json.Marshal(r); return len(b) + OutputTokens }

// The schema is derived from the same typed output contract; nullable pointers are explicit.
func ResponseSchema(r Request) map[string]any {
	if r.PlanDetail == "full" {
		return schema(reflect.TypeOf(Proposal{}), r)
	}
	return schema(reflect.TypeOf(ModelResult{}), r)
}
func schema(t reflect.Type, r Request) map[string]any {
	switch t.Kind() {
	case reflect.Pointer:
		return map[string]any{"anyOf": []any{schema(t.Elem(), r), map[string]any{"type": "null"}}}
	case reflect.Struct:
		properties := map[string]any{}
		required := []string{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := f.Tag.Get("json")
			field := schema(f.Type, r)
			if name == "kind" && t == reflect.TypeOf(ModelResult{}) {
				kinds := []string{"revision"}
				if r.Operation == "propose" {
					kinds = []string{"proposals"}
					if !r.SkipQuestions && r.QuestionsAsked < 2 {
						kinds = append(kinds, "clarification")
					}
				}
				field["enum"] = kinds
			}
			if name == "kind" && t == reflect.TypeOf(Action{}) {
				field["enum"] = actionKinds(r)
			}
			if name == "scene" && t == reflect.TypeOf(Blueprint{}) {
				field["enum"] = []string{"gallery", "table", "walk", "audio"}
			}
			if name == "blueprint" {
				if r.PlanDetail == "full" {
					field = schema(reflect.TypeOf(Blueprint{}), r)
				} else {
					field = map[string]any{"type": "null"}
				}
			}
			if name == "amount" && r.BudgetMaximum == nil {
				field = map[string]any{"type": "null"}
			}
			if name == "evidenceIDs" && t == reflect.TypeOf(Proposal{}) {
				ids := make([]string, 0, len(r.Evidence))
				for _, evidence := range r.Evidence {
					ids = append(ids, evidence.ID)
				}
				if len(ids) == 0 {
					field["maxItems"] = 0
				} else {
					field["items"] = map[string]any{"type": "string", "enum": ids}
				}
			}
			if name == "choices" && t == reflect.TypeOf(Question{}) {
				field["minItems"], field["maxItems"] = 2, 5
			}
			if name == "questions" {
				remaining := 2 - r.QuestionsAsked
				if r.SkipQuestions || r.Operation == "revise" {
					remaining = 0
				}
				field["maxItems"] = remaining
			}
			if name == "budgetUpperBound" && r.BudgetMaximum == nil {
				field = map[string]any{"type": "null"}
			}
			if t == reflect.TypeOf(Proposal{}) {
				if contains([]string{"id", "title", "reason", "budgetNote", "tradeoff", "alternative"}, name) {
					field["minLength"] = 1
				}
				if name == "actions" {
					field["minItems"], field["maxItems"] = 1, 5
					if r.PlanDetail != "" {
						field["maxItems"] = 1
					}
				}
				if name == "preparations" {
					field["maxItems"] = 5
					if r.PlanDetail != "" {
						field["maxItems"] = 0
					}
				}
			}
			if t == reflect.TypeOf(Action{}) && name != "kind" {
				field["minLength"] = 1
			}
			if t == reflect.TypeOf(Preparation{}) && contains([]string{"id", "actionID", "title", "completion"}, name) {
				field["minLength"] = 1
			}
			if t == reflect.TypeOf(Question{}) && contains([]string{"id", "title"}, name) {
				field["minLength"] = 1
			}
			if t == reflect.TypeOf(Blueprint{}) {
				switch name {
				case "premise":
					field["minLength"], field["maxLength"] = 1, 120
				case "preparationMinutes":
					field["minimum"], field["maximum"] = 0, 1440
					if r.Effort == "simple" {
						field["maximum"] = 30
					}
				case "sceneItems":
					field["minItems"], field["maxItems"] = 1, 5
					field["items"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 24}
				case "moments":
					field["minItems"], field["maxItems"] = 1, 5
					if r.Effort == "simple" {
						field["maxItems"] = 3
					}
				case "materials":
					field["maxItems"] = 8
					if r.Effort == "simple" {
						field["maxItems"] = 4
					}
				}
			}
			if t == reflect.TypeOf(Moment{}) {
				switch name {
				case "title":
					field["minLength"], field["maxLength"] = 1, 40
				case "detail":
					field["minLength"], field["maxLength"] = 1, 240
				case "minutes":
					field["minimum"], field["maximum"] = 1, 240
				}
			}
			if t == reflect.TypeOf(Material{}) {
				switch name {
				case "name":
					field["minLength"], field["maxLength"] = 1, 40
				case "detail":
					field["minLength"], field["maxLength"] = 1, 120
				}
			}
			if t == reflect.TypeOf(Keepsake{}) {
				limits := map[string]int{"title": 40, "body": 240, "footer": 80}
				field["minLength"], field["maxLength"] = 1, limits[name]
			}
			properties[name] = field
			required = append(required, name)
		}
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schema(t.Elem(), r)}
	case reflect.Int:
		return map[string]any{"type": "integer"}
	case reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "string"}
	}
}

func (b Blueprint) Validate(r Request, ceiling *float64) error {
	if !contains([]string{"gallery", "table", "walk", "audio"}, b.Scene) || !shortText(b.Premise, 120) || b.PreparationMinutes < 0 || b.PreparationMinutes > 1440 || len(b.SceneItems) < 1 || len(b.SceneItems) > 5 || len(b.Moments) < 1 || len(b.Moments) > 5 || len(b.Materials) > 8 {
		return ErrInvalid
	}
	if r.Effort == "simple" && (b.PreparationMinutes > 30 || len(b.Moments) > 3 || len(b.Materials) > 4) {
		return ErrInvalid
	}
	for _, item := range b.SceneItems {
		if !shortText(item, 24) {
			return ErrInvalid
		}
	}
	duration := 0
	for _, m := range b.Moments {
		duration += m.Minutes
		if !shortText(m.Title, 40) || !shortText(m.Detail, 240) || m.Minutes < 1 || m.Minutes > 240 {
			return ErrInvalid
		}
	}
	if r.Effort == "simple" && duration > 45 {
		return ErrInvalid
	}
	sum := 0.0
	for _, m := range b.Materials {
		if !shortText(m.Name, 40) || !shortText(m.Detail, 120) {
			return ErrInvalid
		}
		if m.Amount != nil {
			if r.BudgetMaximum == nil || ceiling == nil || math.IsNaN(*m.Amount) || math.IsInf(*m.Amount, 0) || *m.Amount < 0 {
				return ErrInvalid
			}
			sum += *m.Amount
		}
	}
	if ceiling != nil && sum > *ceiling+0.001 {
		return ErrInvalid
	}
	if r.BudgetMaximum != nil && sum > *r.BudgetMaximum+0.001 {
		return ErrInvalid
	}
	if !shortText(b.Keepsake.Title, 40) || !shortText(b.Keepsake.Body, 240) || !shortText(b.Keepsake.Footer, 80) {
		return ErrInvalid
	}
	return nil
}
func shortText(s string, limit int) bool {
	return strings.TrimSpace(s) != "" && len([]rune(s)) <= limit
}

// A direct negative restatement is not a proposed activity; keep checking all remaining occurrences.
func containsAffirmative(text, taboo string) bool {
	text, taboo = strings.ToLower(text), strings.ToLower(taboo)
	for _, prefix := range []string{"不安排", "不做", "不要", "避免", "不"} {
		text = strings.ReplaceAll(text, prefix+taboo, "")
	}
	return strings.Contains(text, taboo)
}
func (m *ModelResult) normalizeEmptyCollections() {
	if m.Questions == nil {
		m.Questions = []Question{}
	}
	if m.Changes == nil {
		m.Changes = []string{}
	}
	if m.Proposals == nil {
		m.Proposals = []Proposal{}
	}
	for i := range m.Proposals {
		p := &m.Proposals[i]
		if p.EvidenceIDs == nil {
			p.EvidenceIDs = []string{}
		}
		if p.Preparations == nil {
			p.Preparations = []Preparation{}
		}
		if p.Unknowns == nil {
			p.Unknowns = []string{}
		}
		for j := range p.Preparations {
			if p.Preparations[j].DependsOn == nil {
				p.Preparations[j].DependsOn = []string{}
			}
		}
		if p.Blueprint != nil && p.Blueprint.Materials == nil {
			p.Blueprint.Materials = []Material{}
		}
	}
}
