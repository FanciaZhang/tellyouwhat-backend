package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestSpokenFormatResolutionUsesOnlyOfferedCandidatesAndPendingEvidence(t *testing.T) {
	block, source, receipt := uuid.NewString(), uuid.NewString(), uuid.NewString()
	s := Snapshot{Revision: 2, Blocks: []Block{{ID: block, Text: "早上很好。晚上很好。", Style: "body"}},
		PendingUtterances: []SourceUtterance{{ID: source, Text: "选第二处"}},
		FormatContext: []FormatContext{{ReceiptID: receipt, BlockID: block, State: "pending", Title: "标黄", Quote: "很好",
			Candidates: []FormatCandidate{{ID: "first", Ordinal: 1, Excerpt: "早上「很好」。"}, {ID: "second", Ordinal: 2, Excerpt: "晚上「很好」。"}}}}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	input, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var decoded rewriteModelDocument
	if err := json.Unmarshal(input, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.FormatContext) != 1 || decoded.FormatContext[0].Candidates[1].ID != "second" {
		t.Fatal("candidate context lost")
	}
	resolution := FormatResolution{ID: uuid.NewString(), ReceiptID: receipt, Action: "choose", CandidateID: "second", SourceID: source, Instruction: "选第二处"}
	r := Revision{BaseRevision: 2, FormatResolutions: []FormatResolution{resolution}, ConsumedSourceIDs: []string{source}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*FormatResolution){
		func(v *FormatResolution) { v.CandidateID = "invented" },
		func(v *FormatResolution) { v.SourceID = uuid.NewString() },
		func(v *FormatResolution) { v.Instruction = "没有说过" },
		func(v *FormatResolution) { v.ReceiptID = uuid.NewString() },
		func(v *FormatResolution) { v.Action = "undo"; v.CandidateID = "" },
	} {
		invalid := resolution
		mutate(&invalid)
		r.FormatResolutions = []FormatResolution{invalid}
		if r.Validate(s) == nil {
			t.Fatalf("accepted invalid resolution: %+v", invalid)
		}
	}
	r.FormatResolutions = []FormatResolution{resolution, resolution}
	if r.Validate(s) == nil {
		t.Fatal("duplicate resolution accepted")
	}
	r.FormatResolutions = []FormatResolution{resolution}
	r.BlockEdits = []BlockEdit{{Kind: "replace", ID: block, Text: "改过的正文", Style: "body"}}
	r.Passages = []Passage{{BlockID: block, SourceIDs: []string{source}}}
	if r.Validate(s) == nil {
		t.Fatal("target was rewritten while choosing old candidate")
	}
	r.BlockEdits = nil
	r.Passages = nil
	r.FormatResolutions[0].Action = "dismiss"
	r.FormatResolutions[0].CandidateID = ""
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	s.FormatContext[0].State = "applied"
	s.FormatContext[0].Candidates = nil
	r.FormatResolutions[0].Action = "undo"
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
}

func TestFormatContextIsBoundedAndSchemaRequiresResolutions(t *testing.T) {
	block := uuid.NewString()
	item := FormatContext{ReceiptID: uuid.NewString(), BlockID: block, State: "pending", Quote: "很好"}
	if err := validateFormatContext([]FormatContext{item}, map[string]bool{block: true}); err != nil {
		t.Fatal(err)
	}
	if validateFormatContext([]FormatContext{item, item}, map[string]bool{block: true}) == nil {
		t.Fatal("duplicate context")
	}
	item.Quote = strings.Repeat("字", 121)
	if validateFormatContext([]FormatContext{item}, map[string]bool{block: true}) == nil {
		t.Fatal("unbounded context")
	}
	found := false
	for _, key := range voiceRevisionSchema()["required"].([]string) {
		if key == "formatResolutions" {
			found = true
		}
	}
	if !found {
		t.Fatal("resolution contract missing")
	}
}

func TestGroupedFormatUndoProtectsAndAttributesEveryAffectedBlock(t *testing.T) {
	first, second, source, receipt := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	s := Snapshot{Revision: 2, Blocks: []Block{{ID: first, Text: "带伞", Style: "orderedListItem"}, {ID: second, Text: "带水", Style: "orderedListItem"}},
		PendingUtterances: []SourceUtterance{{ID: source, Text: "撤销刚才那次排版"}},
		FormatContext:     []FormatContext{{ReceiptID: receipt, BlockID: first, AdditionalBlockIDs: []string{second}, State: "applied"}}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	r := Revision{BaseRevision: 2, ConsumedSourceIDs: []string{source}, FormatResolutions: []FormatResolution{{ID: uuid.NewString(), ReceiptID: receipt,
		Action: "undo", SourceID: source, Instruction: "撤销刚才那次排版"}},
		SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: "撤销刚才那次排版", Role: "instruction", BlockIDs: []string{first, second}}}}}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	r.SourcePartitions[0].Segments[0].BlockIDs = []string{first}
	if r.Validate(s) == nil {
		t.Fatal("lost group source attribution")
	}
	r.SourcePartitions = nil
	if validateFormatResolutions(r, s, map[string]bool{second: true}) == nil {
		t.Fatal("rewrote another member during undo")
	}
	s.FormatContext[0].AdditionalBlockIDs = []string{first}
	if s.Validate() == nil {
		t.Fatal("duplicate affected target")
	}
}

func TestExplicitFormatCommandCanStyleUserOwnedTextWithSourceEvidence(t *testing.T) {
	block, source := uuid.NewString(), uuid.NewString()
	s := Snapshot{Revision: 2, Blocks: []Block{{ID: block, Text: "今天很好。", Style: "body"}},
		EditedBlockIDs: []string{block}, PendingUtterances: []SourceUtterance{{ID: source, Text: "刚才那句话加粗"}}}
	command := FormatCommand{ID: uuid.NewString(), BlockID: block, SourceID: source,
		Anchor: TextAnchor{Quote: "今天很好。"}, Instruction: "刚才那句话加粗", Mark: "bold", Enabled: true}
	r := Revision{BaseRevision: 2, FormatCommands: []FormatCommand{command}, ConsumedSourceIDs: []string{source}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*FormatCommand){
		func(c *FormatCommand) { c.Instruction = "伪造指令" },
		func(c *FormatCommand) { c.SourceID = uuid.NewString() },
		func(c *FormatCommand) { c.BlockID = uuid.NewString() },
		func(c *FormatCommand) { c.Mark = "execute" },
		func(c *FormatCommand) { c.Anchor.Quote = "" },
	} {
		bad := command
		mutate(&bad)
		r.FormatCommands = []FormatCommand{bad}
		if r.Validate(s) == nil {
			t.Fatalf("accepted invalid command: %+v", bad)
		}
	}
	r.FormatCommands = []FormatCommand{command, command}
	if r.Validate(s) == nil {
		t.Fatal("duplicate command IDs")
	}
	r.FormatCommands = []FormatCommand{command}
	s.MediaOnlyBlockIDs = []string{block}
	if r.Validate(s) == nil {
		t.Fatal("formatted media-only block")
	}
}

func TestFormatCommandSchemaIsClosedAndRequired(t *testing.T) {
	schema := voiceRevisionSchema()
	required := schema["required"].([]string)
	found := false
	for _, key := range required {
		if key == "formatCommands" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing required commands array")
	}
	properties := schema["properties"].(map[string]any)
	items := properties["formatCommands"].(map[string]any)["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Fatal("open command schema")
	}
}

func TestParagraphFormatRequiresExactWholeBlock(t *testing.T) {
	properties := voiceRevisionSchema()["properties"].(map[string]any)
	items := properties["formatCommands"].(map[string]any)["items"].(map[string]any)
	marks := items["properties"].(map[string]any)["mark"].(map[string]any)["enum"].([]string)
	allowed := map[string]bool{}
	for _, mark := range marks {
		allowed[mark] = true
	}
	block, source := uuid.NewString(), uuid.NewString()
	s := Snapshot{Revision: 2, Blocks: []Block{{ID: block, Text: "隐私保护", Style: "body"}},
		EditedBlockIDs: []string{block}, PendingUtterances: []SourceUtterance{{ID: source, Text: "这段作为二级标题"}}}
	for _, mark := range []string{"heading1", "heading2", "heading3", "body", "orderedListItem", "unorderedListItem"} {
		if !allowed[mark] {
			t.Fatalf("paragraph style missing from model schema: %s", mark)
		}
		command := FormatCommand{ID: uuid.NewString(), BlockID: block, SourceID: source,
			Anchor: TextAnchor{Quote: "隐私保护"}, Instruction: "这段作为二级标题", Mark: mark, Enabled: true}
		r := Revision{BaseRevision: 2, FormatCommands: []FormatCommand{command}, ConsumedSourceIDs: []string{source}}
		if err := r.Validate(s); err != nil {
			t.Fatalf("%s: %v", mark, err)
		}
		for _, mutate := range []func(*FormatCommand){
			func(c *FormatCommand) { c.Anchor.Quote = "隐私" },
			func(c *FormatCommand) { c.Anchor.Prefix = "前文" },
			func(c *FormatCommand) { c.Anchor.Suffix = "后文" },
			func(c *FormatCommand) { c.Enabled = false },
		} {
			bad := command
			mutate(&bad)
			r.FormatCommands = []FormatCommand{bad}
			if r.Validate(s) == nil {
				t.Fatalf("accepted ambiguous paragraph command: %+v", bad)
			}
		}
	}
}
