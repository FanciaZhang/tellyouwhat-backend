package voice

import (
	"github.com/google/uuid"
	"testing"
)

func TestParagraphResolutionEvidenceStateAndTargets(t *testing.T) {
	block, receipt, source := uuid.NewString(), uuid.NewString(), uuid.NewString()
	item := ParagraphContext{ReceiptID: receipt, State: "proposed", Kind: "split", BlockIDs: []string{block}, Instruction: "在晚上前拆段", CanConfirm: true}
	s := Snapshot{Blocks: []Block{{ID: block, Text: "上午出门。晚上回家。"}}, PendingUtterances: []SourceUtterance{{ID: source, Text: "确认拆分"}}, KnownSourceIDs: []string{source}, ParagraphContext: []ParagraphContext{item}}
	answer := ParagraphResolution{ID: uuid.NewString(), ReceiptID: receipt, Action: "confirm", SourceID: source, Instruction: "确认拆分"}
	r := Revision{ParagraphResolutions: []ParagraphResolution{answer}, ConsumedSourceIDs: []string{source}, SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: "确认拆分", Role: "instruction", BlockIDs: []string{block}}}}}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ParagraphResolution){
		"unknown receipt":   func(c *ParagraphResolution) { c.ReceiptID = uuid.NewString() },
		"wrong evidence":    func(c *ParagraphResolution) { c.Instruction = "撤销" },
		"wrong source":      func(c *ParagraphResolution) { c.SourceID = uuid.NewString() },
		"wrong state":       func(c *ParagraphResolution) { c.Action = "undo" },
		"receipt collision": func(c *ParagraphResolution) { c.ID = receipt },
	} {
		t.Run(name, func(t *testing.T) {
			bad := r
			c := answer
			change(&c)
			bad.ParagraphResolutions = []ParagraphResolution{c}
			if bad.Validate(s) == nil {
				t.Fatal("invalid resolution accepted")
			}
		})
	}
	if validateParagraphResolutions(r, s, map[string]bool{block: true}) == nil {
		t.Fatal("overlapping body edit accepted")
	}
	s.ParagraphContext[0].CanConfirm = false
	if r.Validate(s) == nil {
		t.Fatal("stale confirmation accepted")
	}
	r.ParagraphResolutions[0].Action = "dismiss"
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	s.ParagraphContext[0].State = "applied"
	r.ParagraphResolutions[0].Action = "undo"
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	r.SourcePartitions[0].Segments[0].BlockIDs = nil
	if r.Validate(s) == nil {
		t.Fatal("missing evidence targets accepted")
	}
}

func TestParagraphContextAndResolutionSchema(t *testing.T) {
	id := uuid.NewString()
	item := ParagraphContext{ReceiptID: uuid.NewString(), State: "proposed", Kind: "merge", BlockIDs: []string{id}}
	if err := validateParagraphContext([]ParagraphContext{item}, map[string]bool{}); err != nil {
		t.Fatal("stale dismissal context rejected", err)
	}
	item.CanConfirm = true
	if validateParagraphContext([]ParagraphContext{item}, map[string]bool{}) == nil {
		t.Fatal("unknown confirm target accepted")
	}
	found := false
	for _, name := range voiceRevisionSchema()["required"].([]string) {
		if name == "paragraphResolutions" {
			found = true
		}
	}
	if !found {
		t.Fatal("resolution array not required")
	}
}
