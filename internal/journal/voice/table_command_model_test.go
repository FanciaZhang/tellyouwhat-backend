package voice

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCompactTableBuildsIdentityAndProvenanceFromRealEvidence(t *testing.T) {
	s, old := spokenCommandFixture(t, "table")
	if !compactTableCommand(s) {
		t.Fatal("standalone request not routed")
	}
	table := old.TableCreations[0]
	rows := [][]compactTableCell{}
	for _, row := range table.Rows {
		cells := []compactTableCell{}
		for _, c := range row.Cells {
			cells = append(cells, compactTableCell{Kind: c.Kind, Text: c.Text, Number: c.Number, Unit: c.Unit, SourceID: c.Sources[0].SourceID, Quote: c.Sources[0].Anchor.Prefix + c.Sources[0].Anchor.Quote + c.Sources[0].Anchor.Suffix})
		}
		rows = append(rows, cells)
	}
	payload := map[string]any{"tables": []any{map[string]any{"title": table.Title, "afterID": table.AfterID, "columns": []string{"商品", "数量", "金额"}, "rows": rows}}, "questions": []string{}}
	raw, _ := json.Marshal(payload)
	r, err := expandCompactTable(string(raw), s, 17)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.TableCreations) != 1 || len(r.TableCreations[0].Rows) != 3 || r.TranscriptRevision != 17 || len(r.SourcePartitions) != 1 {
		t.Fatal("incomplete table")
	}
	if err = r.Validate(s); err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(r)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(wire, &fields)
	for _, name := range []string{"formatCommands", "paragraphCommands", "timelineCreations", "diagramCreations"} {
		if string(fields[name]) != "[]" {
			t.Fatal("incomplete wire array", name, string(fields[name]))
		}
	}
	rows[0][0].SourceID = "invented-source"
	raw, _ = json.Marshal(payload)
	if _, err = expandCompactTable(string(raw), s, 17); err == nil {
		t.Fatal("fabricated evidence accepted")
	}
}

func TestCompactTableRequestIsBoundedAndDoesNotInterceptMixedOperations(t *testing.T) {
	s, _ := spokenCommandFixture(t, "table")
	prepared, err := PrepareRewrite(context.Background(), s, 0, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Body) > 12000 {
		t.Fatal("simple command carries oversized protocol", len(prepared.Body))
	}
	s.PendingUtterances[0].Text = "今天买了苹果，请整理成表格"
	if compactTableCommand(s) {
		t.Fatal("mixed narration intercepted")
	}
	s.PendingUtterances[0].Text = "请把前面整理成表格和时间线"
	if compactTableCommand(s) {
		t.Fatal("mixed operations intercepted")
	}
}
