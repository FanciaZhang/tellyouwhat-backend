package voice

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func TestCompactNarrativeUsesApprovedFinalModelWithoutLosingSemantics(t *testing.T) {
	s := comparisonSnapshot([]string{"预算280元，钱还没付，明天可能去图书馆。"}, false)
	s.Organization = true
	ctx := promptconfig.WithRevision(t.Context(), promptconfig.Revision{Policy: promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]})
	compact, err := PrepareRewrite(ctx, s, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(compact.Body, &body)
	if body["model"] != "voice" || !strings.Contains(body["instructions"].(string), "否定") || strings.Contains(body["instructions"].(string), "attributionKey") {
		t.Fatal("plain final narrative is not compact and faithful")
	}
	s.Organization = false
	full, err := PrepareRewrite(ctx, s, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(compact.Body) >= len(full.Body)*3/4 {
		t.Fatal("plain request did not meaningfully shrink")
	}
	if s.Polish.Lightweight {
		t.Fatal("final narrative must not be marked as provisional live cleanup")
	}
	for _, mutate := range []func(*Snapshot){
		func(s *Snapshot) { s.Polish.Targets[0].Turns[0].PersonID = uuid.NewString() },
		func(s *Snapshot) { s.Polish.Targets[0].IdentityCorrection = true },
		func(s *Snapshot) { s.Polish.Targets[0].Turns[0].Text = "第一点是价格，第二点是距离。" },
	} {
		s := comparisonSnapshot([]string{"今天去河边。"}, false)
		s.Organization = true
		mutate(&s)
		prepared, err := PrepareRewrite(ctx, s, 1, "")
		if err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(prepared.Body, &body)
		if body["model"] != "voice" {
			t.Fatal("complex or unconfirmed material reached the fast route")
		}
	}
}

type organizationFallbackRewriter struct {
	calls []bool
	stage string
}

func (m *organizationFallbackRewriter) Rewrite(_ context.Context, s Snapshot, _ int) (RewriteResult, error) {
	m.calls = append(m.calls, s.Organization)
	if s.Organization {
		return RewriteResult{Diagnostics: RewriteDiagnostics{Stage: m.stage}}, errors.New("synthetic rejection")
	}
	return RewriteResult{Polish: &PolishRevision{}}, nil
}
func TestCompactFinalFallbackDoesNotRepeatUncertainTransport(t *testing.T) {
	for _, stage := range []string{"validate_polish_missing_source", "http_transport"} {
		m := &organizationFallbackRewriter{stage: stage}
		s := comparisonSnapshot([]string{"还没有付款。"}, false)
		s.Organization = true
		ctx := promptconfig.WithRevision(t.Context(), promptconfig.Revision{Policy: promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]})
		_, _ = (ConfiguredRewriter{Next: m}).Rewrite(ctx, s, 1)
		expected := 1
		if strings.HasPrefix(stage, "validate_polish") {
			expected = 2
		}
		if len(m.calls) != expected {
			t.Fatalf("%s attempts=%d", stage, len(m.calls))
		}
	}
}

func TestOrganizationPlanContinuesAcrossBatchesAndKeepsOriginalProvenance(t *testing.T) {
	texts := make([]string, 25)
	for i := range texts {
		texts[i] = "今天整理了一箱书，预算280元，还没付款。"
	}
	p := OrganizationPlan{Version: OrganizationVersion, Sections: []Snapshot{comparisonSnapshot(texts, false)}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	state := OrganizationState{}
	calls := 0
	for state.Section < len(p.Sections) {
		work, err := state.Work(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(work.Snapshot.Polish.Targets) > 8 {
			t.Fatal("unbounded work")
		}
		paragraphs := []PolishParagraph{}
		ids, text := []string{}, ""
		for _, target := range work.Snapshot.Polish.Targets {
			ids = append(ids, target.ID)
			if len(target.Turns) > 0 {
				text += target.SourceText
			} else {
				text += target.RetainedText
			}
		}
		paragraphs = append(paragraphs, PolishParagraph{Text: text, Style: "body", TargetIDs: ids})
		if err := state.Advance(p, work, &PolishRevision{Paragraphs: paragraphs}); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(state)
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		calls++
		if calls > 25 {
			t.Fatal("no progress")
		}
	}
	if calls < 4 || len(state.Results) != 1 {
		t.Fatal("bounded sequence did not finish")
	}
	result := state.Results[0]
	if len(result.Targets) != 25 || len(result.Paragraphs) != 1 || len(result.Paragraphs[0].TargetIDs) != 25 {
		t.Fatal("source coverage or paragraph continuity lost")
	}
	if result.Paragraphs[0].Text != strings.Join(texts, "") {
		t.Fatal("speech was lost or repeated")
	}
	if result.Targets[0].ID != p.Sections[0].Polish.Targets[0].ID {
		t.Fatal("model-local IDs leaked into the manuscript")
	}
}

func TestOrganizationLongSourceAndManualSectionsRemainBounded(t *testing.T) {
	long := strings.Repeat("预算280元，钱还没付。", 250)
	a, b := comparisonSnapshot([]string{long}, false), comparisonSnapshot([]string{"手动补记之后的新口述。"}, false)
	// The two sections share one grounded author, but not a mergeable document interval.
	b.Narrator = a.Narrator
	b.Polish.Narrator = a.Narrator
	b.Polish.Targets[0].Turns[0].PersonID = a.Narrator.PersonID
	plan := OrganizationPlan{Version: OrganizationVersion, Sections: []Snapshot{a, b}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	state := OrganizationState{}
	for state.Section < 2 {
		work, err := state.Work(plan)
		if err != nil {
			t.Fatal(err)
		}
		ids, text := []string{}, ""
		for _, t := range work.Snapshot.Polish.Targets {
			ids = append(ids, t.ID)
			text += t.RetainedText + t.SourceText
		}
		before, _ := json.Marshal(state)
		if err := state.Advance(plan, work, &PolishRevision{Paragraphs: []PolishParagraph{{Text: "wrong", Style: "body", TargetIDs: []string{uuid.NewString()}}}}); err == nil {
			t.Fatal("accepted a foreign target")
		}
		after, _ := json.Marshal(state)
		if string(before) != string(after) {
			t.Fatal("failed application changed its recovery cursor")
		}
		if err := state.Advance(plan, work, &PolishRevision{Paragraphs: []PolishParagraph{{Text: text, Style: "body", TargetIDs: ids}}}); err != nil {
			t.Fatal(err)
		}
	}
	joined := ""
	for _, paragraph := range state.Results[0].Paragraphs {
		joined += paragraph.Text
	}
	if len(state.Results) != 2 || joined != long || state.Results[1].Paragraphs[0].Text != "手动补记之后的新口述。" {
		t.Fatal("long source or manual boundary changed")
	}
}
