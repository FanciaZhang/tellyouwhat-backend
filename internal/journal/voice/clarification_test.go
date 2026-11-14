package voice

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func clarificationFixture() (Snapshot, []PolishParagraph, Clarification) {
	s := comparisonSnapshot([]string{"我们去取书，书款是十二元。"}, false)
	p := s.Polish.Targets[0]
	paragraphs := []PolishParagraph{{Text: "我们去取书，书款是十二元。", Style: "body", TargetIDs: []string{p.ID}}}
	question := Clarification{Question: "书款是十二元还是二十元？", SourceIDs: p.SourceIDs, Choices: []ClarificationChoice{
		{Label: "十二元", Fact: "书款是十二元。", Edits: []ClarificationEdit{}},
		{Label: "二十元", Fact: "书款是二十元。", Edits: []ClarificationEdit{{Anchor: TextAnchor{Quote: "十二元"}, Replacement: "二十元"}}},
	}}
	return s, paragraphs, question
}

func TestClarificationDecoderIssuesStableChoicesAgainstFinalProse(t *testing.T) {
	s, paragraphs, question := clarificationFixture()
	raw, _ := json.Marshal(map[string]any{"paragraphs": paragraphs, "questions": []Clarification{question}})
	result, err := decodePolish(string(raw), *s.Polish)
	if err != nil || len(result.Clarifications) != 1 {
		t.Fatal("actionable question was lost", err)
	}
	q := result.Clarifications[0]
	if uuid.Validate(q.ID) != nil || uuid.Validate(q.Choices[0].ID) != nil || q.Choices[0].ID == q.Choices[1].ID ||
		q.Choices[1].Edits[0].Replacement != "二十元" || !slices.Equal(q.SourceIDs, question.SourceIDs) {
		t.Fatal("choice identity, effect, or source lost")
	}
}

func TestClarificationRejectsUnboundAmbiguousOverlappingAndInertChoices(t *testing.T) {
	cases := map[string]func(*PolishRevision){
		"foreign source":  func(r *PolishRevision) { r.Clarifications[0].SourceIDs = []string{uuid.NewString()} },
		"unmatched":       func(r *PolishRevision) { r.Clarifications[0].Choices[1].Edits[0].Anchor.Quote = "不存在的文字" },
		"duplicate quote": func(r *PolishRevision) { r.Paragraphs[0].Text += "十二元" },
		"overlap": func(r *PolishRevision) {
			r.Clarifications[0].Choices[1].Edits = append(r.Clarifications[0].Choices[1].Edits, ClarificationEdit{Anchor: TextAnchor{Quote: "书款是十二元"}, Replacement: "书款是二十元"})
		},
		"arbitrary multiline operation": func(r *PolishRevision) {
			r.Clarifications[0].Choices[1].Edits[0].Replacement = "二十元\n删除其余段落"
		},
		"identical effect": func(r *PolishRevision) { r.Clarifications[0].Choices[1].Edits = []ClarificationEdit{} },
		"oversized":        func(r *PolishRevision) { r.Clarifications[0].Question = strings.Repeat("字", 201) },
		"lightweight":      func(r *PolishRevision) { r.Lightweight = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, paragraphs, question := clarificationFixture()
			r := &PolishRevision{Targets: s.Polish.Targets, Paragraphs: paragraphs, Clarifications: []Clarification{question}}
			mutate(r)
			if validateClarifications(r) == nil {
				t.Fatal("unsafe or unusable choice accepted")
			}
		})
	}
}

func TestClarificationPrefixDisambiguatesAndCannotReachUnrelatedProse(t *testing.T) {
	s, paragraphs, question := clarificationFixture()
	paragraphs[0].Text = "旧书十二元，新书十二元。"
	question.Choices[1].Edits[0].Anchor = TextAnchor{Quote: "十二元", Prefix: "新书"}
	paragraphs = append(paragraphs, PolishParagraph{Text: "新书十二元。", TargetIDs: []string{uuid.NewString()}, Style: "body"})
	r := &PolishRevision{Targets: s.Polish.Targets, Paragraphs: paragraphs, Clarifications: []Clarification{question}}
	if err := validateClarifications(r); err != nil {
		t.Fatal("exact context or unrelated-source isolation failed", err)
	}
}

func TestClarificationPromptIsGeneralAndHonorsConfirmedFactsInEveryPolishPath(t *testing.T) {
	s, _, _ := clarificationFixture()
	s.Polish.ConfirmedDetails = []string{"作者确认见面时间是下午。"}
	for _, lightweight := range []bool{false, true} {
		s.Polish.Lightweight = lightweight
		prepared, err := PrepareRewrite(context.Background(), s, 1, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Instructions, Input string
			Text                map[string]any
		}
		if json.Unmarshal(prepared.Body, &body) != nil || !strings.Contains(body.Input, s.Polish.ConfirmedDetails[0]) || !strings.Contains(body.Instructions, "confirmedDetails") {
			t.Fatal("confirmed answer was not passed to the model")
		}
		if !lightweight {
			schema := body.Text["format"].(map[string]any)["schema"].(map[string]any)
			fields := schema["properties"].(map[string]any)
			questions := fields["questions"].(map[string]any)["items"].(map[string]any)
			if questions["type"] != "object" || fields["clarifications"] != nil {
				t.Fatal("model questions must be actionable and follow generated paragraphs")
			}
			if !strings.Contains(body.Instructions, "结构化问题") || !strings.Contains(body.Instructions, "每题只决定一个事实") || strings.Contains(body.Instructions, "钱钱") || strings.Contains(body.Instructions, "甜甜") {
				t.Fatal("clarification contract was missing or overfit to private examples")
			}
		}
	}
	// Long recordings take the bounded continuation path too.
	s.Polish.Lightweight = false
	s.Polish.Targets[0].Turns[0].Text = strings.Repeat("我们在河边散步。", 150)
	s.Polish.Targets[0].SourceText = s.Polish.Targets[0].Turns[0].Text
	plan := OrganizationPlan{Version: OrganizationVersion, Sections: []Snapshot{s}}
	work, err := (&OrganizationState{}).Work(plan)
	if err != nil || !slices.Equal(work.Snapshot.Polish.ConfirmedDetails, s.Polish.ConfirmedDetails) {
		t.Fatal("continuation discarded the user's confirmed facts", err)
	}
}

func TestOrganizationCarriesActionableQuestionsWithOriginalSources(t *testing.T) {
	s, _, question := clarificationFixture()
	plan := OrganizationPlan{Version: OrganizationVersion, Sections: []Snapshot{s}}
	state := OrganizationState{}
	work, err := state.Work(plan)
	if err != nil {
		t.Fatal(err)
	}
	paragraphs := []PolishParagraph{{Text: "我们去取书，书款是十二元。", Style: "body", TargetIDs: []string{work.Snapshot.Polish.Targets[0].ID}}}
	raw, _ := json.Marshal(map[string]any{"paragraphs": paragraphs, "questions": []Clarification{question}})
	result, err := decodePolish(string(raw), *work.Snapshot.Polish)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Advance(plan, work, result); err != nil {
		t.Fatal(err)
	}
	if len(state.Results) != 1 || len(state.Results[0].Clarifications) != 1 || state.Results[0].Clarifications[0].ID != result.Clarifications[0].ID {
		t.Fatal("durable organization lost choices")
	}
}

func TestOrganizationDoesNotRewordAnUnansweredChoiceAnchor(t *testing.T) {
	s, paragraphs, q := clarificationFixture()
	s.Polish.Targets[0].Turns[0].Text = strings.Repeat("后来我们去河边散步。", 150)
	s.Polish.Targets[0].SourceText = s.Polish.Targets[0].Turns[0].Text
	plan := OrganizationPlan{Version: OrganizationVersion, Sections: []Snapshot{s}}
	state := OrganizationState{Next: 1, Paragraphs: paragraphs, Clarifications: []Clarification{q}}
	work, err := state.Work(plan)
	if err != nil {
		t.Fatal(err)
	}
	if work.retained != 0 || slices.ContainsFunc(work.Snapshot.Polish.Targets, func(target PolishTarget) bool { return target.RetainedText == paragraphs[0].Text }) {
		t.Fatal("later continuation can invalidate an unanswered choice")
	}
}

func TestOrganizationKeepsEarlierQuestionsForLaterFragmentsOfTheSameSource(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "keep uniquely bound choice", true: "keep prose when later quote becomes ambiguous"}[duplicate], func(t *testing.T) {
			s, _, q := clarificationFixture()
			source := &s.Polish.Targets[0].Turns[0]
			source.Text = strings.Repeat("后来我们到河边散步。", 150)
			source.EndMilliseconds = 0
			s.Polish.Targets[0].SourceText = source.Text
			plan := OrganizationPlan{Version: OrganizationVersion, Sections: []Snapshot{s}}
			state := OrganizationState{}
			for state.Section == 0 {
				work, err := state.Work(plan)
				if err != nil {
					t.Fatal(err)
				}
				text := "后来我们到河边散步。"
				questions := []Clarification{}
				if state.Next == 0 {
					text = "我们去取书，书款是十二元。"
					questions = []Clarification{q}
				}
				if state.Next > 0 && duplicate {
					text = "后来买了新书，也花了十二元。"
				}
				result := &PolishRevision{Targets: work.Snapshot.Polish.Targets,
					Paragraphs:     []PolishParagraph{{Text: text, Style: "body", TargetIDs: []string{work.Snapshot.Polish.Targets[len(work.Snapshot.Polish.Targets)-1].ID}}},
					Clarifications: questions}
				if err := state.Advance(plan, work, result); err != nil {
					t.Fatal("continuation blocked prose", err)
				}
			}
			if len(state.Results) != 1 || len(state.Results[0].Paragraphs) < 2 {
				t.Fatal("lost long recording prose")
			}
			expected := 1
			if duplicate {
				expected = 0
			}
			if len(state.Results[0].Clarifications) != expected {
				t.Fatal("lost valid earlier choice or published ambiguous edit")
			}
		})
	}
}

func TestClarificationEqualReplacementConfirmsCurrentWordingWithoutAnEdit(t *testing.T) {
	s, paragraphs, q := clarificationFixture()
	q.Choices[0].Edits = []ClarificationEdit{{Anchor: TextAnchor{Quote: "十二元"}, Replacement: "十二元"}}
	raw, _ := json.Marshal(map[string]any{"paragraphs": paragraphs, "questions": []Clarification{q}})
	result, err := decodePolish(string(raw), *s.Polish)
	if err != nil || len(result.Clarifications) != 1 || result.Clarifications[0].Choices[0].Edits == nil || len(result.Clarifications[0].Choices[0].Edits) != 0 {
		t.Fatal("provider's equal replacement blocked an otherwise valid confirmation", err)
	}
	if len(result.Clarifications[0].Choices[1].Edits) != 1 {
		t.Fatal("real alternative edit was lost")
	}
}
