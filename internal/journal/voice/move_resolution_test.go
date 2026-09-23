package voice

import (
	"github.com/google/uuid"
	"testing"
)

func TestMoveResolutionCapabilitiesEvidenceAndSourceTargets(t *testing.T) {
	a, b, source, receipt := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	s := Snapshot{Blocks: []Block{{ID: a, Text: "甲"}, {ID: b, Text: "乙"}}, PendingUtterances: []SourceUtterance{{ID: source, Text: "确认这次移动"}},
		MoveContext: []MoveContext{{ReceiptID: receipt, State: "proposed", BlockIDs: []string{a, b}, CanConfirm: true}}}
	resolution := MoveResolution{ID: uuid.NewString(), ReceiptID: receipt, Action: "confirm", SourceID: source, Instruction: "确认这次移动"}
	r := Revision{MoveResolutions: []MoveResolution{resolution}, ConsumedSourceIDs: []string{source}, SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: "确认这次移动", Role: "instruction", BlockIDs: []string{a, b}}}}}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	if err := validateMoveContext(s.MoveContext, map[string]bool{a: true, b: true}); err != nil {
		t.Fatal(err)
	}
	s.MoveContext[0].CanConfirm = false
	if r.Validate(s) == nil {
		t.Fatal("stale confirmation accepted")
	}
	r.MoveResolutions[0].Action = "dismiss"
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	r.SourcePartitions[0].Segments[0].BlockIDs = []string{a}
	if r.Validate(s) == nil {
		t.Fatal("incomplete target evidence")
	}
	r.SourcePartitions = nil
	r.MoveResolutions[0].Action = "undo"
	if r.Validate(s) == nil {
		t.Fatal("undo of proposed preview")
	}
	s.MoveContext[0].State = "applied"
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	if validateMoveResolutions(r, s, map[string]bool{b: true}) == nil {
		t.Fatal("target overwrite collision")
	}
	r.MoveResolutions[0].Instruction = "未说过"
	if r.Validate(s) == nil {
		t.Fatal("missing evidence accepted")
	}
}

func TestMoveResolutionSchemaAndDuplicateActions(t *testing.T) {
	required := false
	for _, key := range voiceRevisionSchema()["required"].([]string) {
		if key == "moveResolutions" {
			required = true
		}
	}
	if !required {
		t.Fatal("missing schema field")
	}
	block, receipt, source := uuid.NewString(), uuid.NewString(), uuid.NewString()
	s := Snapshot{MoveContext: []MoveContext{{ReceiptID: receipt, State: "proposed", BlockIDs: []string{block}, CanConfirm: true}}, PendingUtterances: []SourceUtterance{{ID: source, Text: "确认"}}}
	c := MoveResolution{ID: uuid.NewString(), ReceiptID: receipt, Action: "confirm", SourceID: source, Instruction: "确认"}
	r := Revision{MoveResolutions: []MoveResolution{c, c}, ConsumedSourceIDs: []string{source}}
	if validateMoveResolutions(r, s, nil) == nil {
		t.Fatal("duplicate receipt accepted")
	}
	c.ID = receipt
	r.MoveResolutions = []MoveResolution{c}
	if validateMoveResolutions(r, s, nil) == nil {
		t.Fatal("receipt identity reused as resolution")
	}
}
