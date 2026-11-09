package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"testing"
)

func TestParagraphSplitAndMergeValidation(t *testing.T) {
	a, b, c, source, media := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	speech := "在晚上前拆段"
	s := Snapshot{Blocks: []Block{{ID: a, Text: "上午出门。晚上回家。"}, {ID: b, Text: "准备休息。"}, {ID: c, Text: "结尾"}},
		PendingUtterances: []SourceUtterance{{ID: source, Text: speech}}, KnownSourceIDs: []string{source}, BlockComponents: map[string][]string{a: {media}}}
	command := ParagraphCommand{ID: uuid.NewString(), Kind: "split", BlockIDs: []string{a}, Anchor: TextAnchor{Quote: "晚上"}, Edge: "before", ComponentsToSecond: []string{media}, SourceID: source, Instruction: speech}
	r := Revision{ParagraphCommands: []ParagraphCommand{command}, ConsumedSourceIDs: []string{source}, SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: speech, Role: "instruction", BlockIDs: []string{a}}}}}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ParagraphCommand){
		"missing quote":     func(c *ParagraphCommand) { c.Anchor.Quote = "不存在" },
		"outer boundary":    func(c *ParagraphCommand) { c.Anchor.Quote = "上午" },
		"unknown component": func(c *ParagraphCommand) { c.ComponentsToSecond = []string{uuid.NewString()} },
		"unknown block":     func(c *ParagraphCommand) { c.BlockIDs = []string{uuid.NewString()} },
		"missing evidence":  func(c *ParagraphCommand) { c.Instruction = "没有说过" },
		"wrong edge":        func(c *ParagraphCommand) { c.Edge = "middle" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := command
			change(&bad)
			trial := r
			trial.ParagraphCommands = []ParagraphCommand{bad}
			if trial.Validate(s) == nil {
				t.Fatal("invalid command accepted")
			}
		})
	}
	if err := validateParagraphs(r, s, map[string]bool{a: true}); err == nil {
		t.Fatal("body collision accepted")
	}
	r.SourcePartitions[0].Segments[0].BlockIDs = []string{b}
	if r.Validate(s) == nil {
		t.Fatal("wrong instruction targets accepted")
	}
	r.SourcePartitions = nil
	merge := ParagraphCommand{ID: uuid.NewString(), Kind: "merge", BlockIDs: []string{a, b}, SourceID: source, Instruction: speech}
	r.ParagraphCommands = []ParagraphCommand{merge}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	r.ParagraphCommands[0].BlockIDs = []string{a, c}
	if r.Validate(s) == nil {
		t.Fatal("nonadjacent merge accepted")
	}
	r.ParagraphCommands = []ParagraphCommand{merge}
	s.ParallelGroups = [][]string{{a, b, c}}
	s.ParallelColumns = map[string]string{a: "group:0", b: "group:0", c: "group:1"}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	s.ParallelColumns[b] = "group:1"
	if r.Validate(s) == nil {
		t.Fatal("cross-column merge accepted")
	}
}

func TestParagraphBoundaryAndSchema(t *testing.T) {
	if _, ok := paragraphBoundary("上午晚上，晚上回家", TextAnchor{Quote: "晚上"}, "before"); ok {
		t.Fatal("ambiguous anchor accepted")
	}
	if _, ok := paragraphBoundary("上午晚上，晚上回家", TextAnchor{Quote: "晚上", Suffix: "回家"}, "before"); !ok {
		t.Fatal("contextual anchor rejected")
	}
	found := false
	for _, name := range voiceRevisionSchema()["required"].([]string) {
		if name == "paragraphCommands" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing required paragraphCommands")
	}
	s := Snapshot{Blocks: []Block{{ID: uuid.NewString(), Text: "正文"}}}
	data, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err = json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"blockComponents", "parallelColumns"} {
		if _, ok := input[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
}

func TestParagraphMetadataIsValidatedAndContextBounded(t *testing.T) {
	s := Snapshot{BlockComponents: map[string][]string{}, ParallelColumns: map[string]string{}}
	for i := 0; i < 12; i++ {
		id := uuid.NewString()
		s.Blocks = append(s.Blocks, Block{ID: id, Text: "独立正文"})
		s.BlockComponents[id] = []string{uuid.NewString()}
	}
	data, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var input rewriteModelDocument
	if err = json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	if len(input.BlockComponents) > len(input.ContextBlocks) {
		t.Fatal("metadata exceeds context window")
	}
	a, b, c := s.Blocks[0].ID, s.Blocks[1].ID, s.Blocks[2].ID
	s.ParallelGroups = [][]string{{a, b, c}}
	s.ParallelColumns = map[string]string{a: "left", b: "left", c: "right"}
	if err = s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.ParallelColumns[b] = "right"
	s.ParallelColumns[c] = "left"
	if s.Validate() == nil {
		t.Fatal("interleaved columns accepted")
	}
	s.ParallelColumns = map[string]string{s.Blocks[3].ID: "unrelated"}
	if s.Validate() == nil {
		t.Fatal("ungrouped column accepted")
	}
}
