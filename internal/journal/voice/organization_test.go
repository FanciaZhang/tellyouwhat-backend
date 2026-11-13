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

func TestOrganizationUsesTheSameGroundedFinalizationAsWorkbench(t *testing.T) {
	s := comparisonSnapshot([]string{"周三，啊，周五去图书馆。还没付款，不是已经付款。"}, false)
	ctx := promptconfig.WithRevision(t.Context(), promptconfig.Revision{Policy: promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]})
	normal, err := PrepareRewrite(ctx, s, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Organization = true
	organized, err := PrepareRewrite(ctx, s, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(normal.Body) != string(organized.Body) {
		t.Fatal("mobile organization silently replaced the validated finalization prompt")
	}
	var body map[string]any
	if json.Unmarshal(organized.Body, &body) != nil {
		t.Fatal("invalid request")
	}
	instructions := body["instructions"].(string)
	for _, rule := range []string{"最后明确确认", "还没付款", "他人告知", "checks"} {
		if !strings.Contains(instructions, rule) {
			t.Fatalf("missing finalization rule: %s", rule)
		}
	}
	if body["text"].(map[string]any)["format"].(map[string]any)["name"] != "journal_voice_narrative_v37" {
		t.Fatal("mobile request used an obsolete narrative schema")
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
func TestOrganizationDoesNotRepeatTheSameRejectedPrompt(t *testing.T) {
	for _, stage := range []string{"validate_polish_missing_source", "http_transport"} {
		m := &organizationFallbackRewriter{stage: stage}
		s := comparisonSnapshot([]string{"还没有付款。"}, false)
		s.Organization = true
		ctx := promptconfig.WithRevision(t.Context(), promptconfig.Revision{Policy: promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]})
		_, _ = (ConfiguredRewriter{Next: m}).Rewrite(ctx, s, 1)
		if len(m.calls) != 1 {
			t.Fatalf("%s attempts=%d", stage, len(m.calls))
		}
	}
}

func TestOrganizationUsesAudioChronologyInsteadOfOldParagraphPositions(t *testing.T) {
	s := comparisonSnapshot([]string{"最后下山回家。", "先到山脚等日出。", "然后沿着山路登顶。"}, false)
	for i, start := range []int{2000, 0, 1000} {
		s.Polish.Targets[i].Turns[0].StartMilliseconds = start
		s.Polish.Targets[i].Turns[0].EndMilliseconds = start + 900
	}
	before, _ := json.Marshal(s)
	fragments := organizationFragments(s)
	for i, expected := range []string{"先到山脚等日出。", "然后沿着山路登顶。", "最后下山回家。"} {
		if fragments[i].Target.SourceText != expected {
			t.Fatalf("fragment %d: %s", i, fragments[i].Target.SourceText)
		}
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("chronological preparation changed the saved document")
	}
	plan := OrganizationPlan{Version: OrganizationVersion, Sections: []Snapshot{s}}
	state := OrganizationState{}
	work, err := state.Work(plan)
	if err != nil {
		t.Fatal(err)
	}
	paragraphs := []PolishParagraph{}
	for _, target := range work.Snapshot.Polish.Targets {
		paragraphs = append(paragraphs, PolishParagraph{Text: target.SourceText, Style: "body", TargetIDs: []string{target.ID}})
	}
	if err := state.Advance(plan, work, &PolishRevision{Paragraphs: paragraphs}); err != nil {
		t.Fatal(err)
	}
	for i, original := range []int{1, 2, 0} {
		if len(state.Results[0].Paragraphs[i].TargetIDs) != 1 || state.Results[0].Paragraphs[i].TargetIDs[0] != s.Polish.Targets[original].ID {
			t.Fatal("ordering detached prose from its original source")
		}
	}
}

func TestOrganizationPreservesProvidedOrderWithoutUnambiguousTiming(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		s := comparisonSnapshot([]string{"先描述眼前。", "再回忆过去。"}, false)
		if mixed {
			s.Polish.Targets[0].Turns[0].StartMilliseconds = 2000
			s.Polish.Targets[0].Turns[0].EndMilliseconds = 3000
		}
		fragments := organizationFragments(s)
		if fragments[0].Target.SourceText != "先描述眼前。" || fragments[1].Target.SourceText != "再回忆过去。" {
			t.Fatal("invented chronology for untimed source")
		}
	}
}

func TestFinalNarrativeEndingUsesAudioTimeAfterParagraphMoves(t *testing.T) {
	s := comparisonSnapshot([]string{"最后我决定按时归还图书。", "先去图书馆寻找图鉴。", "随后管理员帮我找到了图鉴。"}, false)
	paragraphs := []PolishParagraph{}
	for i, start := range []int{2000, 0, 1000} {
		s.Polish.Targets[i].Turns[0].StartMilliseconds = start
		s.Polish.Targets[i].Turns[0].EndMilliseconds = start + 900
		paragraphs = append(paragraphs, PolishParagraph{Text: s.Polish.Targets[i].SourceText, Style: "body", TargetIDs: []string{s.Polish.Targets[i].ID}})
	}
	preservePolishEnding(*s.Polish, nil, paragraphs)
	if paragraphs[len(paragraphs)-1].Text != "最后我决定按时归还图书。" {
		t.Fatal("old paragraph position replaced the actual closing thought")
	}
}

func TestCompleteTimedNarrativeSeesWholeSourceInOneBoundedCall(t *testing.T) {
	texts := make([]string, 40)
	for i := range texts {
		texts[i] = "今天去图书馆借植物图鉴。"
	}
	s := comparisonSnapshot(texts, false)
	for i := range s.Polish.Targets {
		s.Polish.Targets[i].Turns[0].StartMilliseconds = i * 1000
		s.Polish.Targets[i].Turns[0].EndMilliseconds = i*1000 + 900
	}
	s.Polish.Targets[0], s.Polish.Targets[39] = s.Polish.Targets[39], s.Polish.Targets[0]
	plan := OrganizationPlan{Version: OrganizationVersion, Sections: []Snapshot{s}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	work, err := (OrganizationState{}).Work(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !work.Snapshot.Polish.wholeNarrative || len(work.Snapshot.Polish.Targets) != 40 || work.Next != len(organizationFragments(s)) {
		t.Fatal("short complete source was fragmented into isolated rewrites")
	}
	if work.Snapshot.Polish.Targets[0].Turns[0].StartMilliseconds != 0 {
		t.Fatal("whole source did not follow audio chronology")
	}
	ordinary := *work.Snapshot.Polish
	ordinary.wholeNarrative = false
	if ordinary.Validate() == nil {
		t.Fatal("ordinary public polish accepted an oversized request")
	}
	ctx := promptconfig.WithRevision(t.Context(), promptconfig.Revision{Policy: promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]})
	prepared, err := PrepareRewrite(ctx, work.Snapshot, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if json.Unmarshal(prepared.Body, &body) != nil {
		t.Fatal("invalid request")
	}
	if prepared.Parameters.MaxOutputTokens > 8192 || prepared.TimeoutSeconds > 90 {
		t.Fatal("whole source has an unbounded output or timeout")
	}
	state := OrganizationState{}
	paragraph := PolishParagraph{Text: strings.Join(texts, ""), Style: "body", TargetIDs: []string{}}
	for _, target := range work.Snapshot.Polish.Targets {
		paragraph.TargetIDs = append(paragraph.TargetIDs, target.ID)
	}
	raw, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{paragraph}, "questions": []string{}, "omissions": []PolishOmission{}, "illustrationSuggestion": nil, "checks": []string{}})
	decoded, err := decodePolish(string(raw), *work.Snapshot.Polish)
	if err != nil {
		t.Fatal("valid whole-source references could not be decoded", err)
	}
	if err := state.Advance(plan, work, decoded); err != nil {
		t.Fatal(err)
	}
	if state.Section != 1 || len(state.Results[0].Paragraphs[0].TargetIDs) != 40 {
		t.Fatal("whole pass lost original target references")
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
