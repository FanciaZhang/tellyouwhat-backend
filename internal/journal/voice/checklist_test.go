package voice

import (
	"github.com/google/uuid"
	"testing"
)

func TestChecklistSinglePreviewRequiresOfferedConfirmationAndEvidence(t *testing.T) {
	block, source, receipt := uuid.NewString(), uuid.NewString(), uuid.NewString()
	s := Snapshot{Revision: 2, Blocks: []Block{{ID: block, Text: "带雨伞", Style: "checklistItem"}},
		PendingUtterances: []SourceUtterance{{ID: source, Text: "确认完成"}},
		FormatContext:     []FormatContext{{ReceiptID: receipt, BlockID: block, State: "proposed", Title: "标记清单项为完成", Quote: "带雨伞", CanConfirm: true}}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	r := Revision{BaseRevision: 2, ConsumedSourceIDs: []string{source}, FormatResolutions: []FormatResolution{{ID: uuid.NewString(), ReceiptID: receipt, SourceID: source, Instruction: "确认完成", Action: "confirm"}}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	s.FormatContext[0].CanConfirm = false
	if r.Validate(s) == nil {
		t.Fatal("accepted stale confirmation")
	}
	s.FormatContext[0].CanConfirm = true
	r.FormatResolutions[0].Instruction = "没有说过"
	if r.Validate(s) == nil {
		t.Fatal("accepted invented evidence")
	}
	r.FormatResolutions[0].Instruction = "确认完成"
	r.FormatResolutions[0].Action = "dismiss"
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
}

func TestChecklistCommandsRequireWholeItemAndRecordedSource(t *testing.T) {
	for _, style := range []string{"checklistItem", "completedChecklistItem"} {
		block, source := uuid.NewString(), uuid.NewString()
		s := Snapshot{Revision: 2, Blocks: []Block{{ID: block, Text: "带雨伞", Style: style}}, PendingUtterances: []SourceUtterance{{ID: source, Text: "雨伞这项标记完成"}}}
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
		r := Revision{BaseRevision: 2, ConsumedSourceIDs: []string{source}, FormatCommands: []FormatCommand{{ID: uuid.NewString(), BlockID: block, SourceID: source, Instruction: "雨伞这项标记完成", Anchor: TextAnchor{Quote: "带雨伞"}, Mark: style, Enabled: true}}}
		if err := r.Validate(s); err != nil {
			t.Fatal(err)
		}
		r.FormatCommands[0].SourceID = uuid.NewString()
		if r.Validate(s) == nil {
			t.Fatal("accepted unrecorded checklist command")
		}
	}
}
