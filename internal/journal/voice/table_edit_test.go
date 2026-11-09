package voice

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func tableEditFixture() (Snapshot, Revision) {
	s := tableContextFixture()
	t := s.TableContext[0]
	source := s.PendingUtterances[0].ID
	instruction := "把笔记本价格改成49.90元"
	s.PendingUtterances[0].Text = instruction
	cell := t.Rows[0].Cells[1]
	cell.Number = "49.90"
	cell.Sources = []TableSource{{SourceID: source, Anchor: TextAnchor{Quote: "49.90元"}}}
	r := Revision{TableEdits: []TableEdit{{ID: uuid.NewString(), BlockID: t.BlockID, TableID: t.TableID, SourceID: source, Instruction: instruction,
		Patches: []TablePatch{{Kind: "setCell", TargetID: t.Rows[0].ID, Cell: &cell}}}}, ConsumedSourceIDs: []string{source},
		SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: instruction, Role: "instruction", BlockIDs: []string{t.BlockID}}}}}}
	return s, r
}

func TestNumericSortRevisionRequiresInstructionAuthorization(t *testing.T) {
	s, r := tableEditFixture()
	table := &s.TableContext[0]
	row := TableRow{ID: uuid.NewString(), Cells: append([]TableCell(nil), table.Rows[0].Cells...)}
	row.Cells[1].Number = "2.00"
	table.Rows = append(table.Rows, row)
	instruction := "按价格从低到高排列"
	s.PendingUtterances[0].Text = instruction
	r.TableEdits[0].Instruction = instruction
	r.TableEdits[0].Patches = []TablePatch{{Kind: "sortNumbersAscending", TargetID: table.Columns[1].ID}}
	r.SourcePartitions[0].Segments[0].Text = instruction
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	r.SourcePartitions[0].Segments[0].Role = "content"
	if err := r.Validate(s); err == nil {
		t.Fatal("unauthorized sort accepted")
	}
}

func TestTableEditRevisionRequiresScopedInstructionAndEvidence(t *testing.T) {
	s, r := tableEditFixture()
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Revision){
		"unknown table":          func(r *Revision) { r.TableEdits[0].TableID = uuid.NewString() },
		"wrong owner":            func(r *Revision) { r.TableEdits[0].BlockID = uuid.NewString() },
		"missing instruction":    func(r *Revision) { r.TableEdits[0].Instruction = "" },
		"fabricated instruction": func(r *Revision) { r.TableEdits[0].Instruction = "改为100元" },
		"unconsumed source":      func(r *Revision) { r.ConsumedSourceIDs = nil },
		"missing evidence":       func(r *Revision) { r.TableEdits[0].Patches[0].Cell.Sources = nil },
		"fabricated evidence":    func(r *Revision) { r.TableEdits[0].Patches[0].Cell.Sources[0].Anchor.Quote = "100元" },
		"duplicate evidence":     func(r *Revision) { c := r.TableEdits[0].Patches[0].Cell; c.Sources = append(c.Sources, c.Sources[0]) },
		"missing partition":      func(r *Revision) { r.SourcePartitions = nil },
		"instruction as prose":   func(r *Revision) { r.SourcePartitions[0].Segments[0].Role = "content" },
		"duplicate target":       func(r *Revision) { r.TableEdits = append(r.TableEdits, r.TableEdits[0]) },
		"identity collision":     func(r *Revision) { r.TableEdits[0].ID = s.TableContext[0].Rows[0].ID },
		"no change":              func(r *Revision) { r.TableEdits[0].Patches[0].Cell.Number = "39.90" },
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(r)
			var copy Revision
			_ = json.Unmarshal(data, &copy)
			change(&copy)
			if copy.Validate(s) == nil {
				t.Fatal("invalid edit accepted")
			}
		})
	}
}

func TestTableEditSeparateContentEvidenceAndStrictSchema(t *testing.T) {
	unchangedSnapshot, unchanged := tableEditFixture()
	unchanged.TableEdits[0].Patches[0].Cell.Number = "39.90"
	for i := range unchangedSnapshot.TableContext[0].Rows[0].Cells {
		unchangedSnapshot.TableContext[0].Rows[0].Cells[i].Sources = []TableSource{}
	}
	if unchanged.Validate(unchangedSnapshot) == nil {
		t.Fatal("empty source arrays bypassed no-op check")
	}
	s, r := tableEditFixture()
	e := &r.TableEdits[0]
	e.Instruction = "把笔记本价格改成"
	s.PendingUtterances[0].Text = e.Instruction + "49.90元"
	r.SourcePartitions[0].Segments = []SourceSegment{{Text: e.Instruction, Role: "instruction", BlockIDs: []string{e.BlockID}}, {Text: "49.90元", Role: "content", BlockIDs: []string{e.BlockID}}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	r.SourcePartitions[0].Segments[1] = SourceSegment{Text: "49.90元", Role: "context"}
	if r.Validate(s) == nil {
		t.Fatal("unrelated context authorized value")
	}
	schema := voiceRevisionSchema()
	if !slices.Contains(schema["required"].([]string), "tableEdits") {
		t.Fatal("missing required edits")
	}
	edit := schema["properties"].(map[string]any)["tableEdits"].(map[string]any)["items"].(map[string]any)
	if edit["additionalProperties"] != false || !slices.Contains(edit["required"].([]string), "patches") {
		t.Fatal("loose schema")
	}
}
