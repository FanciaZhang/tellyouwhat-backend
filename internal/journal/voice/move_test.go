package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"testing"
)

func TestMoveValidationAndExpandedSourceAttribution(t *testing.T) {
	a, b, c, source := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	speech := "把第一段移到结论后面"
	s := Snapshot{Blocks: []Block{{ID: a, Text: "背景"}, {ID: b, Text: "补充"}, {ID: c, Text: "结论"}},
		ParallelGroups: [][]string{{a, b}}, PendingUtterances: []SourceUtterance{{ID: source, Text: speech}}, KnownSourceIDs: []string{source}}
	command := MoveCommand{ID: uuid.NewString(), BlockIDs: []string{a}, AfterID: &c, SourceID: source, Instruction: speech}
	r := Revision{MoveCommands: []MoveCommand{command}, ConsumedSourceIDs: []string{source},
		SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: speech, Role: "instruction", BlockIDs: []string{a, b}}}}}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	if err := validateParallelGroups(s); err != nil {
		t.Fatal(err)
	}
	r.SourcePartitions[0].Segments[0].BlockIDs = []string{a}
	if r.Validate(s) == nil {
		t.Fatal("missing expanded provenance accepted")
	}
	r.SourcePartitions = nil
	for _, bad := range []MoveCommand{
		{ID: command.ID, BlockIDs: []string{a}, AfterID: &b, SourceID: source, Instruction: speech},
		{ID: command.ID, BlockIDs: []string{a, a}, AfterID: &c, SourceID: source, Instruction: speech},
		{ID: command.ID, BlockIDs: []string{a}, AfterID: &c, SourceID: source, Instruction: "没有说过"},
		{ID: command.ID, BlockIDs: []string{a}, SourceID: source, Instruction: speech},
	} {
		r.MoveCommands = []MoveCommand{bad}
		if r.Validate(s) == nil {
			t.Fatal("invalid move accepted")
		}
	}
	r.MoveCommands = []MoveCommand{command}
	if validateMoves(r, s, map[string]bool{b: true}) == nil {
		t.Fatal("parallel member patch collision accepted")
	}
	s.ParallelGroups = [][]string{{a, c}}
	if validateParallelGroups(s) == nil {
		t.Fatal("noncontiguous group accepted")
	}
}

func TestMoveSchemaRequiresTypedCommands(t *testing.T) {
	found := false
	for _, name := range voiceRevisionSchema()["required"].([]string) {
		if name == "moveCommands" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing required moveCommands")
	}
	s := Snapshot{Blocks: []Block{{ID: uuid.NewString(), Text: "正文"}}}
	data, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	if _, found := input["parallelGroups"]; !found {
		t.Fatal("missing structural context")
	}
}
