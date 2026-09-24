package voice

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestTablePatchesPreserveUntouchedDataAndApplyStructuralEdits(t *testing.T) {
	before := tableContextFixture().TableContext[0]
	original, _ := json.Marshal(before)
	column := TableColumn{ID: uuid.NewString(), Title: "备注"}
	cell := before.Rows[0].Cells[1]
	cell.Number = "49.90"
	cell.Sources = []TableSource{{SourceID: uuid.NewString(), Anchor: TextAnchor{Quote: "49.90元"}}}
	row := TableRow{ID: uuid.NewString(), Cells: []TableCell{
		{ColumnID: before.Columns[0].ID, Kind: "text", Text: "咖啡"},
		{ColumnID: before.Columns[1].ID, Kind: "pending"},
		{ColumnID: column.ID, Kind: "pending"},
	}}
	after, err := applyTablePatches(before, []TablePatch{
		{Kind: "setCell", TargetID: before.Rows[0].ID, Cell: &cell},
		{Kind: "insertColumn", TargetID: before.Columns[1].ID, Column: &column},
		{Kind: "insertRow", TargetID: before.Rows[0].ID, Row: &row},
		{Kind: "renameColumn", TargetID: column.ID, Title: "说明"},
		{Kind: "orderRows", TargetID: before.TableID, Order: []string{row.ID, before.Rows[0].ID}},
		{Kind: "orderColumns", TargetID: before.TableID, Order: []string{column.ID, before.Columns[1].ID, before.Columns[0].ID}},
		{Kind: "renameTable", TargetID: before.TableID, Title: "周末采购"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.Rows[1].Cells[1].Number != "49.90" || !reflect.DeepEqual(after.Rows[1].Cells[1].Sources, cell.Sources) ||
		after.Columns[0].Title != "说明" || !reflect.DeepEqual(after.Rows[1].Cells[0], before.Rows[0].Cells[0]) {
		t.Fatal("lost edit, evidence or untouched value")
	}
	unchanged, _ := json.Marshal(before)
	if string(original) != string(unchanged) {
		t.Fatal("mutated snapshot")
	}
	deleted, err := applyTablePatches(after, []TablePatch{{Kind: "deleteRow", TargetID: row.ID}, {Kind: "deleteColumn", TargetID: column.ID}})
	if err != nil || len(deleted.Rows) != 1 || len(deleted.Columns) != 2 || len(deleted.Rows[0].Cells) != 2 {
		t.Fatal("deletion shape", err)
	}
}

func TestTablePatchesRejectInvalidBatchesWithoutChangingSnapshot(t *testing.T) {
	before := tableContextFixture().TableContext[0]
	original, _ := json.Marshal(before)
	for name, patches := range map[string][]TablePatch{
		"unknown row":        {{Kind: "deleteRow", TargetID: uuid.NewString()}},
		"missing payload":    {{Kind: "setCell", TargetID: before.Rows[0].ID}},
		"ignored payload":    {{Kind: "deleteRow", TargetID: before.Rows[0].ID, Title: "ignored"}},
		"duplicate order":    {{Kind: "orderColumns", TargetID: before.TableID, Order: []string{before.Columns[0].ID, before.Columns[0].ID}}},
		"incomplete order":   {{Kind: "orderColumns", TargetID: before.TableID, Order: []string{before.Columns[0].ID}}},
		"identity reuse":     {{Kind: "deleteRow", TargetID: before.Rows[0].ID}, {Kind: "insertRow", Row: &before.Rows[0]}},
		"remove last column": {{Kind: "deleteColumn", TargetID: before.Columns[0].ID}, {Kind: "deleteColumn", TargetID: before.Columns[1].ID}},
		"partial row":        {{Kind: "insertRow", Row: &TableRow{ID: uuid.NewString()}}},
		"wrong target":       {{Kind: "renameTable", TargetID: before.BlockID, Title: "名字"}},
		"blank column":       {{Kind: "renameColumn", TargetID: before.Columns[0].ID, Title: " "}},
		"unknown operation":  {{Kind: "rewriteEverything"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := applyTablePatches(before, patches); err == nil {
				t.Fatal("invalid batch accepted")
			}
			current, _ := json.Marshal(before)
			if string(original) != string(current) {
				t.Fatal("failure mutated snapshot")
			}
		})
	}
}

func TestNumericTableSortUsesExactValuesAndStableMissingTail(t *testing.T) {
	before := tableContextFixture().TableContext[0]
	column := before.Columns[1].ID
	original := before.Rows[0]
	makeRow := func(number string, review bool) TableRow {
		row := TableRow{ID: uuid.NewString(), Cells: append([]TableCell(nil), original.Cells...)}
		row.Cells[1].Number = number
		row.Cells[1].NeedsReview = review
		if number == "" {
			row.Cells[1] = TableCell{ColumnID: column, Kind: "pending"}
		}
		return row
	}
	before.Rows = []TableRow{makeRow("10", false), makeRow("", false), makeRow("2", false), makeRow("2.00", false), makeRow("1", true)}
	encoded, _ := json.Marshal(before)
	for _, test := range []struct {
		kind  string
		order []int
	}{
		{"sortNumbersAscending", []int{2, 3, 0, 1, 4}},
		{"sortNumbersDescending", []int{0, 2, 3, 1, 4}},
	} {
		after, err := applyTablePatches(before, []TablePatch{{Kind: test.kind, TargetID: column}})
		if err != nil {
			t.Fatal(err)
		}
		for index, originalIndex := range test.order {
			if !reflect.DeepEqual(after.Rows[index], before.Rows[originalIndex]) {
				t.Fatal("lost stable order or cell provenance")
			}
		}
	}
	unchanged, _ := json.Marshal(before)
	if string(encoded) != string(unchanged) {
		t.Fatal("mutated snapshot")
	}
	before.Rows[2].Cells[1].Unit = "美元"
	if _, err := applyTablePatches(before, []TablePatch{{Kind: "sortNumbersAscending", TargetID: column}}); err == nil {
		t.Fatal("mixed units accepted")
	}
	before.Rows[2].Cells[1] = TableCell{ColumnID: column, Kind: "text", Text: "两元"}
	if _, err := applyTablePatches(before, []TablePatch{{Kind: "sortNumbersAscending", TargetID: column}}); err == nil {
		t.Fatal("mixed types accepted")
	}
}
