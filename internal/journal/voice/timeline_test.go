package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"slices"
	"testing"
)

func TestTimelineProposalEvidenceTimeAndIdentity(t *testing.T) {
	block, source, target := uuid.NewString(), uuid.NewString(), uuid.NewString()
	content, instruction := "早上散步。", "整理成时间线。"
	s := Snapshot{Blocks: []Block{{ID: block, Text: "一天"}}, PendingUtterances: []SourceUtterance{{ID: source, Text: content + instruction}}}
	r := Revision{ConsumedSourceIDs: []string{source}, SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{
		{Text: content, Role: "content", BlockIDs: []string{target}}, {Text: instruction, Role: "instruction", BlockIDs: []string{target}},
	}}}}
	c := TimelineCreation{ID: uuid.NewString(), BlockID: target, TimelineID: uuid.NewString(), AfterID: &block, SourceID: source,
		Instruction: instruction, Title: "早晨", Events: []TimelineEvent{{ID: uuid.NewString(), Title: "散步", TimeExpression: "早上",
			Precision: "period", Period: "morning", Intent: "experience", Sources: []TableSource{{SourceID: source, Anchor: TextAnchor{Quote: "早上散步"}}}}}}
	if err := validateTimelineCreations([]TimelineCreation{c}, r, s); err != nil {
		t.Fatal(err)
	}
	r.TimelineCreations = []TimelineCreation{c}
	contextSnapshot := s
	contextSnapshot.TimelineContext = []TimelineContext{{TimelineID: uuid.NewString(), Events: []TimelineEvent{{ID: c.Events[0].ID}}}}
	if err := validateTimelineCreations([]TimelineCreation{c}, r, contextSnapshot); err == nil {
		t.Fatal("creation reused an existing timeline event identity")
	}
	if err := r.Validate(s); err != nil {
		t.Fatalf("full revision: %v", err)
	}
	if !slices.Contains(voiceRevisionSchema()["required"].([]string), "timelineCreations") {
		t.Fatal("timeline missing from required schema")
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var restored Revision
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if err := restored.Validate(s); err != nil {
		t.Fatal(err)
	}
	t.Run("existing table row identity", func(t *testing.T) {
		occupied := s
		occupied.TableContext = []TableContext{{Rows: []TableRow{{ID: c.Events[0].ID}}}}
		if err := validateTimelineCreations([]TimelineCreation{c}, r, occupied); err == nil {
			t.Fatal("reused existing row ID")
		}
	})
	t.Run("pending receipt identity", func(t *testing.T) {
		occupied := s
		occupied.TableReceiptContext = []TableReceiptContext{{ReceiptID: c.ID}}
		if err := validateTimelineCreations([]TimelineCreation{c}, r, occupied); err == nil {
			t.Fatal("reused receipt ID")
		}
	})
	t.Run("same revision table calculation identity", func(t *testing.T) {
		mixed := r
		mixed.TableEdits = []TableEdit{{ID: uuid.NewString(), Patches: []TablePatch{{Calculation: &TableCalculation{ID: c.Events[0].ID}}}}}
		if err := validateTimelineCreations([]TimelineCreation{c}, mixed, s); err == nil {
			t.Fatal("reused new calculation ID")
		}
	})
	for name, change := range map[string]func(*TimelineCreation){
		"fabricated evidence":     func(v *TimelineCreation) { v.Events[0].Sources[0].Anchor.Quote = "跑步" },
		"instruction as event":    func(v *TimelineCreation) { v.Events[0].Sources[0].Anchor.Quote = instruction },
		"unknown source":          func(v *TimelineCreation) { v.Events[0].Sources[0].SourceID = uuid.NewString() },
		"invalid day":             func(v *TimelineCreation) { v.Events[0].Day = "2026-02-30" },
		"unknown precision":       func(v *TimelineCreation) { v.Events[0].Precision = "exact" },
		"contradictory precision": func(v *TimelineCreation) { minute := 600; v.Events[0].Minute = &minute },
		"unknown intent":          func(v *TimelineCreation) { v.Events[0].Intent = "maybe" },
		"cycle":                   func(v *TimelineCreation) { v.Events[0].AfterEventID = &v.Events[0].ID },
		"unknown relative event":  func(v *TimelineCreation) { id := uuid.NewString(); v.Events[0].AfterEventID = &id },
		"duplicate identity":      func(v *TimelineCreation) { v.Events[0].ID = v.ID },
		"existing identity":       func(v *TimelineCreation) { v.ID = block },
		"unknown reference":       func(v *TimelineCreation) { v.Events[0].BlockIDs = []string{uuid.NewString()} },
		"duplicate source":        func(v *TimelineCreation) { v.Events[0].Sources = append(v.Events[0].Sources, v.Events[0].Sources[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(c)
			var candidate TimelineCreation
			if err := json.Unmarshal(data, &candidate); err != nil {
				t.Fatal(err)
			}
			change(&candidate)
			if err := validateTimelineCreations([]TimelineCreation{candidate}, r, s); err == nil {
				t.Fatal("accepted invalid timeline")
			}
		})
	}
	if err := validateTimelineCreations([]TimelineCreation{c, c}, r, s); err == nil {
		t.Fatal("accepted duplicate proposal")
	}
	r.SourcePartitions[0].Segments[1].Role = "content"
	if err := validateTimelineCreations([]TimelineCreation{c}, r, s); err == nil {
		t.Fatal("accepted instruction as content")
	}
}
