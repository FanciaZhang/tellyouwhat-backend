package voice

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func tableFixture() (Snapshot, Revision) {
	block, source, target := uuid.NewString(), uuid.NewString(), uuid.NewString()
	name, price := uuid.NewString(), uuid.NewString()
	instruction, content := "把采购整理成表格", "。笔记本39.90元。"
	evidence := func(quote string) []TableSource {
		return []TableSource{{SourceID: source, Anchor: TextAnchor{Quote: quote}}}
	}
	c := TableCreation{ID: uuid.NewString(), BlockID: target, TableID: uuid.NewString(), AfterID: &block, SourceID: source, Instruction: instruction, Title: "采购",
		Columns: []TableColumn{{ID: name, Title: "项目"}, {ID: price, Title: "价格"}}, Rows: []TableRow{{ID: uuid.NewString(), Cells: []TableCell{
			{ColumnID: name, Kind: "text", Text: "笔记本", Sources: evidence("笔记本")},
			{ColumnID: price, Kind: "number", Number: "39.90", Unit: "元", Sources: evidence("39.90元")},
		}}}}
	s := Snapshot{Blocks: []Block{{ID: block, Text: "周末计划"}}, PendingUtterances: []SourceUtterance{{ID: source, Text: instruction + content}}, KnownSourceIDs: []string{source}}
	r := Revision{TableCreations: []TableCreation{c}, ConsumedSourceIDs: []string{source}, SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{
		{Text: instruction, Role: "instruction", BlockIDs: []string{target}}, {Text: content, Role: "content", BlockIDs: []string{target}},
	}}}}
	return s, r
}

func TestTableCreationSourceShapeNumbersAndIdentity(t *testing.T) {
	s, r := tableFixture()
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Revision){
		"missing cell evidence": func(v *Revision) { v.TableCreations[0].Rows[0].Cells[0].Sources = nil },
		"fabricated quote":      func(v *Revision) { v.TableCreations[0].Rows[0].Cells[0].Sources[0].Anchor.Quote = "钢笔" },
		"unknown source":        func(v *Revision) { v.TableCreations[0].Rows[0].Cells[0].Sources[0].SourceID = uuid.NewString() },
		"instruction as data": func(v *Revision) {
			v.TableCreations[0].Rows[0].Cells[0].Sources[0].Anchor.Quote = v.TableCreations[0].Instruction
		},
		"duplicate source": func(v *Revision) {
			cell := &v.TableCreations[0].Rows[0].Cells[0]
			cell.Sources = append(cell.Sources, cell.Sources[0])
		},
		"scientific notation": func(v *Revision) { v.TableCreations[0].Rows[0].Cells[1].Number = "39e2" },
		"partial decimal":     func(v *Revision) { v.TableCreations[0].Rows[0].Cells[1].Number = "39.9元" },
		"too many digits":     func(v *Revision) { v.TableCreations[0].Rows[0].Cells[1].Number = strings.Repeat("9", 29) },
		"pending with number": func(v *Revision) { v.TableCreations[0].Rows[0].Cells[1].Kind = "pending" },
		"wrong row shape":     func(v *Revision) { v.TableCreations[0].Rows[0].Cells = v.TableCreations[0].Rows[0].Cells[:1] },
		"duplicate column":    func(v *Revision) { v.TableCreations[0].Columns[1].ID = v.TableCreations[0].Columns[0].ID },
		"duplicate cell": func(v *Revision) {
			v.TableCreations[0].Rows[0].Cells[1].ColumnID = v.TableCreations[0].Rows[0].Cells[0].ColumnID
		},
		"existing block identity": func(v *Revision) { v.TableCreations[0].BlockID = s.Blocks[0].ID },
		"unknown destination":     func(v *Revision) { id := uuid.NewString(); v.TableCreations[0].AfterID = &id },
		"missing instruction partition": func(v *Revision) {
			v.SourcePartitions[0].Segments[0].Role = "context"
			v.SourcePartitions[0].Segments[0].BlockIDs = nil
		},
		"wrong content target": func(v *Revision) { v.SourcePartitions[0].Segments[1].BlockIDs = []string{s.Blocks[0].ID} },
		"missing partitions":   func(v *Revision) { v.SourcePartitions = nil },
		"too many rows": func(v *Revision) {
			for len(v.TableCreations[0].Rows) < 65 {
				v.TableCreations[0].Rows = append(v.TableCreations[0].Rows, v.TableCreations[0].Rows[0])
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(r)
			var bad Revision
			if err := json.Unmarshal(raw, &bad); err != nil {
				t.Fatal(err)
			}
			change(&bad)
			if bad.Validate(s) == nil {
				t.Fatal("invalid table accepted")
			}
		})
	}
	r.TableCreations[0].Rows[0].Cells[1] = TableCell{ColumnID: r.TableCreations[0].Columns[1].ID, Kind: "pending", Sources: []TableSource{}}
	if err := r.Validate(s); err != nil {
		t.Fatal("pending value rejected", err)
	}
}

func TestTableCreationPartitionsSchemaAndContextAnchors(t *testing.T) {
	s, r := tableFixture()
	c := r.TableCreations[0]
	if !slices.Contains(voiceRevisionSchema()["required"].([]string), "tableCreations") {
		t.Fatal("missing required array")
	}
	props := voiceRevisionSchema()["properties"].(map[string]any)
	schema := props["tableCreations"].(map[string]any)["items"].(map[string]any)
	for _, field := range []string{"id", "blockID", "tableID", "afterID", "columns", "rows", "sourceID", "instruction"} {
		if !slices.Contains(schema["required"].([]string), field) {
			t.Fatal("missing field", field)
		}
	}
	s.BlockComponents = map[string][]string{s.Blocks[0].ID: {c.TableID}}
	if r.Validate(s) == nil {
		t.Fatal("component collision accepted")
	}
	s.BlockComponents = nil
	r.FormatCommands = []FormatCommand{{ID: c.ID}}
	if validateTables(r, s) == nil {
		t.Fatal("operation collision accepted")
	}
	r.FormatCommands = nil
	r.ParagraphCommands = []ParagraphCommand{{ID: uuid.NewString()}}
	if validateTables(r, s) == nil {
		t.Fatal("simultaneous structure mutation accepted")
	}
	start, end, ok := tableAnchorRange("笔记本和笔记本", TextAnchor{Quote: "笔记本"})
	if ok {
		t.Fatal("ambiguous anchor accepted", start, end)
	}
	start, end, ok = tableAnchorRange("笔记本和笔记本", TextAnchor{Quote: "笔记本", Prefix: "和"})
	if !ok || start != len("笔记本和") || end != len("笔记本和笔记本") {
		t.Fatal("context anchor failed")
	}
}
