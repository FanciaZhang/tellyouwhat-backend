package voice

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"slices"
	"strings"
	"testing"
)

func TestPolishSemanticStylesKeepListsAndConclusionOnWire(t *testing.T) {
	s := polishFixture()
	s.Polish.Targets[0].Text = "第一点，准备材料。第二点，确认时间。最后检查。"
	s.Polish.Targets[0].SourceText = s.Polish.Targets[0].Text
	id := s.Polish.Targets[0].ID
	paragraphs := []PolishParagraph{
		{Text: "接下来的安排", TargetIDs: []string{id}, Style: "heading2"},
		{Text: "准备材料。", TargetIDs: []string{id}, Style: "orderedListItem"},
		{Text: "确认时间。", TargetIDs: []string{id}, Style: "orderedListItem"},
		{Text: "最后检查。", TargetIDs: []string{id}, Style: "body"},
		{Text: "保留完整解释。", TargetIDs: []string{id}, Style: "body"},
	}
	raw, _ := json.Marshal(map[string]any{"paragraphs": paragraphs, "questions": []string{}})
	revision, err := decodePolish(string(raw), *s.Polish)
	if err != nil || len(revision.Paragraphs) != 5 {
		t.Fatal("semantic result rejected", err)
	}
	if revision.Paragraphs[1].Style != "orderedListItem" || revision.Paragraphs[3].Style != "body" {
		t.Fatal("semantic styles lost")
	}
	prepared, err := PrepareRewrite(context.Background(), s, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(prepared.Body, &body)
	schema := body["text"].(map[string]any)["format"].(map[string]any)["schema"].(map[string]any)
	item := schema["properties"].(map[string]any)["paragraphs"].(map[string]any)["items"].(map[string]any)
	required := item["required"].([]any)
	if !slices.Contains(required, any("style")) || !strings.Contains(string(prepared.Body), "默认不主动添加 emoji") {
		t.Fatal("structure or expression constraint omitted")
	}
	for _, style := range []string{"", "markdown", "emotion"} {
		paragraphs[0].Style = style
		raw, _ = json.Marshal(map[string]any{"paragraphs": paragraphs, "questions": []string{}})
		if _, err := decodePolish(string(raw), *s.Polish); err == nil {
			t.Fatal("invalid style accepted", style)
		}
	}
}

func TestTableCrossSentenceContextReachesModelAndValidatesEvidence(t *testing.T) {
	s, r := tableFixture()
	command := r.TableCreations[0].SourceID
	old := uuid.NewString()
	content := r.SourcePartitions[0].Segments[1].Text
	s.PendingUtterances[0].Text = r.TableCreations[0].Instruction
	s.KnownSourceIDs = append(s.KnownSourceIDs, old)
	s.TableSourceContext = []TableSource{{SourceID: old, Anchor: TextAnchor{Quote: content}}}
	r.SourcePartitions[0].Segments = r.SourcePartitions[0].Segments[:1]
	for index := range r.TableCreations[0].Rows[0].Cells {
		r.TableCreations[0].Rows[0].Cells[index].Sources[0].SourceID = old
	}
	if err := r.Validate(s); err != nil {
		t.Fatal("cross-sentence table rejected", err)
	}
	input, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var model rewriteModelDocument
	_ = json.Unmarshal(input, &model)
	if len(model.TableSourceContext) != 1 || model.TableSourceContext[0].SourceID != old {
		t.Fatal("model lost contextual evidence")
	}
	if !slices.Equal(r.ConsumedSourceIDs, []string{command}) {
		t.Fatal("historical data consumed again")
	}
	for name, change := range map[string]func(*Snapshot){
		"missing context":        func(s *Snapshot) { s.TableSourceContext = nil },
		"unknown source":         func(s *Snapshot) { s.TableSourceContext[0].SourceID = uuid.NewString() },
		"instruction as context": func(s *Snapshot) { s.TableSourceContext[0].Anchor.Quote = r.TableCreations[0].Instruction },
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(s)
			var bad Snapshot
			_ = json.Unmarshal(raw, &bad)
			change(&bad)
			if r.Validate(bad) == nil {
				t.Fatal("unauthorized source accepted")
			}
		})
	}
}

func TestTimelineCrossSentenceContextKeepsEventEvidenceAndInstructionSeparate(t *testing.T) {
	block, command, old, target := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	instruction := "把前面的行程整理成时间线。"
	s := Snapshot{Blocks: []Block{{ID: block, Text: "早上去了公园散步。", Style: "body"}}, KnownSourceIDs: []string{command, old},
		PendingUtterances: []SourceUtterance{{ID: command, Text: instruction}}, TimelineSourceContext: []TableSource{{SourceID: old, Anchor: TextAnchor{Quote: "早上去了公园散步。"}}}}
	c := TimelineCreation{ID: uuid.NewString(), BlockID: target, TimelineID: uuid.NewString(), AfterID: &block, SourceID: command, Instruction: instruction, Title: "一天的行程",
		Events: []TimelineEvent{{ID: uuid.NewString(), Title: "公园散步", TimeExpression: "早上", Precision: "period", Period: "morning", Intent: "experience",
			Sources: []TableSource{{SourceID: old, Anchor: TextAnchor{Quote: "早上去了公园散步"}}}}}}
	r := Revision{TimelineCreations: []TimelineCreation{c}, ConsumedSourceIDs: []string{command}, SourcePartitions: []SourcePartition{{SourceID: command,
		Segments: []SourceSegment{{Text: instruction, Role: "instruction", BlockIDs: []string{target}}}}}}
	if err := r.Validate(s); err != nil {
		t.Fatal("cross-sentence timeline rejected", err)
	}
	input, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var model rewriteModelDocument
	_ = json.Unmarshal(input, &model)
	if len(model.TimelineSourceContext) != 1 {
		t.Fatal("model lost timeline context")
	}
	s.TimelineSourceContext = nil
	if r.Validate(s) == nil {
		t.Fatal("missing event evidence accepted")
	}
}
