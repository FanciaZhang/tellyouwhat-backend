package voice

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func tableResolutionFixture() (Snapshot, Revision) {
	s := tableContextFixture()
	table := s.TableContext[0]
	source := s.PendingUtterances[0].ID
	s.PendingUtterances[0].Text = "就按这个表格修改"
	c := TableReceiptContext{ReceiptID: uuid.NewString(), State: "proposed", Kind: "edit", BlockID: table.BlockID, TableID: table.TableID, Title: table.Title, Instruction: "修改预算", CanConfirm: true}
	s.TableReceiptContext = []TableReceiptContext{c}
	r := Revision{TableResolutions: []TableResolution{{ID: uuid.NewString(), ReceiptID: c.ReceiptID, Action: "confirm", SourceID: source, Instruction: s.PendingUtterances[0].Text}}, ConsumedSourceIDs: []string{source},
		SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: s.PendingUtterances[0].Text, Role: "instruction", BlockIDs: []string{table.BlockID}}}}}}
	return s, r
}

func TestTableResolutionLifecycleAndModelProjection(t *testing.T) {
	s, r := tableResolutionFixture()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	data, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var projected rewriteModelDocument
	if err = json.Unmarshal(data, &projected); err != nil || !reflect.DeepEqual(s.TableReceiptContext, projected.TableReceiptContext) {
		t.Fatal("lost receipt context", err)
	}
	s.TableReceiptContext[0].State = "applied"
	s.TableReceiptContext[0].CanConfirm = false
	s.TableReceiptContext[0].CanUndo = true
	r.TableResolutions[0].Action = "undo"
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	s.TableReceiptContext[0].State = "undone"
	s.TableReceiptContext[0].CanUndo = false
	s.TableReceiptContext[0].CanConfirm = true
	r.TableResolutions[0].Action = "confirm"
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	// A proposed creation's block is not yet part of the document.
	s.TableReceiptContext[0].Kind = "creation"
	s.TableReceiptContext[0].State = "proposed"
	s.TableReceiptContext[0].BlockID = uuid.NewString()
	s.TableReceiptContext[0].TableID = uuid.NewString()
	r.SourcePartitions[0].Segments[0].BlockIDs = []string{s.TableReceiptContext[0].BlockID}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	s.TableReceiptContext[0].CanConfirm = false
	r.TableResolutions[0].Action = "dismiss"
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(voiceRevisionSchema()["required"].([]string), "tableResolutions") {
		t.Fatal("missing output contract")
	}
}

func TestTableResolutionRejectsStaleUnknownAndConflictingCommands(t *testing.T) {
	for name, change := range map[string]func(*Snapshot, *Revision){
		"stale confirm": func(s *Snapshot, r *Revision) { s.TableReceiptContext[0].CanConfirm = false },
		"unsafe undo": func(s *Snapshot, r *Revision) {
			s.TableReceiptContext[0].State = "applied"
			s.TableReceiptContext[0].CanConfirm = false
			r.TableResolutions[0].Action = "undo"
		},
		"unknown receipt":        func(s *Snapshot, r *Revision) { r.TableResolutions[0].ReceiptID = uuid.NewString() },
		"duplicate receipt":      func(s *Snapshot, r *Revision) { r.TableResolutions = append(r.TableResolutions, r.TableResolutions[0]) },
		"identity collision":     func(s *Snapshot, r *Revision) { r.TableResolutions[0].ID = s.Blocks[0].ID },
		"missing consumption":    func(s *Snapshot, r *Revision) { r.ConsumedSourceIDs = nil },
		"fabricated instruction": func(s *Snapshot, r *Revision) { r.TableResolutions[0].Instruction = "从未说过" },
		"missing partition":      func(s *Snapshot, r *Revision) { r.SourcePartitions = nil },
		"wrong partition": func(s *Snapshot, r *Revision) {
			r.SourcePartitions[0].Segments[0].BlockIDs = []string{uuid.NewString()}
		},
		"unrecognized action": func(s *Snapshot, r *Revision) { r.TableResolutions[0].Action = "delete" },
	} {
		t.Run(name, func(t *testing.T) {
			s, r := tableResolutionFixture()
			change(&s, &r)
			if r.Validate(s) == nil {
				t.Fatal("invalid resolution accepted")
			}
		})
	}
	for name, change := range map[string]func(*Snapshot){
		"duplicate receipt":       func(s *Snapshot) { s.TableReceiptContext = append(s.TableReceiptContext, s.TableReceiptContext[0]) },
		"impossible state":        func(s *Snapshot) { s.TableReceiptContext[0].State = "dismissed" },
		"false capability":        func(s *Snapshot) { s.TableReceiptContext[0].CanUndo = true },
		"missing editable target": func(s *Snapshot) { s.TableReceiptContext[0].BlockID = uuid.NewString() },
		"wrong owner":             func(s *Snapshot) { s.TableReceiptContext[0].TableID = uuid.NewString() },
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := tableResolutionFixture()
			change(&s)
			if s.Validate() == nil {
				t.Fatal("invalid context accepted")
			}
		})
	}
}
