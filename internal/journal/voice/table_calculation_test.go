package voice

import (
	"github.com/google/uuid"
	"reflect"
	"testing"
)

func TestCalculationPatchPreservesSourceDataAndRequiresValidOperands(t *testing.T) {
	before := tableContextFixture().TableContext[0]
	column := before.Columns[1].ID
	second := TableRow{ID: uuid.NewString(), Cells: append([]TableCell(nil), before.Rows[0].Cells...)}
	second.Cells[1].Number = "2.00"
	second.Cells[1].Approximate = true
	before.Rows = append(before.Rows, second)
	calculation := TableCalculation{ID: uuid.NewString(), Title: "费用合计", Kind: "sum", ColumnID: column, RowIDs: []string{}}
	patch := TablePatch{Kind: "addCalculation", TargetID: before.TableID, Calculation: &calculation}
	after, err := applyTablePatches(before, []TablePatch{patch})
	if err != nil || len(after.Calculations) != 1 || !reflect.DeepEqual(after.Rows, before.Rows) || len(before.Calculations) != 0 {
		t.Fatal("expression changed source data", err)
	}
	if _, err := applyTablePatches(after, []TablePatch{patch}); err == nil {
		t.Fatal("duplicate calculation identity accepted")
	}
	calculation.Kind = "difference"
	calculation.RowIDs = []string{before.Rows[0].ID, second.ID}
	if _, err := applyTablePatches(before, []TablePatch{patch}); err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]string{{second.ID}, {second.ID, second.ID}, {second.ID, uuid.NewString()}} {
		calculation.RowIDs = ids
		if _, err := applyTablePatches(before, []TablePatch{patch}); err == nil {
			t.Fatal("invalid operands accepted")
		}
	}
	calculation.RowIDs = []string{before.Rows[0].ID, second.ID}
	before.Rows[1].Cells[1].NeedsReview = true
	if _, err := applyTablePatches(before, []TablePatch{patch}); err == nil {
		t.Fatal("unconfirmed difference accepted")
	}
	calculation.Kind = "sum"
	calculation.RowIDs = []string{}
	if _, err := applyTablePatches(before, []TablePatch{patch}); err != nil {
		t.Fatal("sum should exclude review row", err)
	}
	before.Rows[0].Cells[1].NeedsReview = true
	if _, err := applyTablePatches(before, []TablePatch{patch}); err == nil {
		t.Fatal("sum with no confirmed values accepted")
	}
	before.Rows[0].Cells[1].NeedsReview = false
	before.Rows[1].Cells[1].NeedsReview = false
	before.Rows[1].Cells[1].Unit = "美元"
	if _, err := applyTablePatches(before, []TablePatch{patch}); err == nil {
		t.Fatal("mixed units accepted")
	}
	patch.Kind = "renameTable"
	if _, err := applyTablePatches(before, []TablePatch{patch}); err == nil {
		t.Fatal("unused calculation payload accepted")
	}
}

func TestCalculationRevisionRequiresInstructionAndReservesIdentity(t *testing.T) {
	s, r := tableEditFixture()
	table := &s.TableContext[0]
	c := TableCalculation{ID: uuid.NewString(), Title: "价格合计", Kind: "sum", ColumnID: table.Columns[1].ID, RowIDs: []string{}}
	instruction := "把价格合计一下"
	s.PendingUtterances[0].Text = instruction
	r.TableEdits[0].Instruction = instruction
	r.SourcePartitions[0].Segments[0].Text = instruction
	r.TableEdits[0].Patches = []TablePatch{{Kind: "addCalculation", TargetID: table.TableID, Calculation: &c}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	r.SourcePartitions[0].Segments[0].Role = "content"
	if err := r.Validate(s); err == nil {
		t.Fatal("calculation without instruction accepted")
	}
	r.SourcePartitions[0].Segments[0].Role = "instruction"
	table.Calculations = []TableCalculation{c}
	if err := r.Validate(s); err == nil {
		t.Fatal("existing calculation identity reused")
	}
	// Deleted operands remain representable, not silently dropped or cached.
	table.Calculations[0].ColumnID = uuid.NewString()
	if err := validateTableContext(s); err != nil {
		t.Fatal("invalidated existing expression was lost", err)
	}
	table.Calculations[0].ID = table.Rows[0].ID
	if err := validateTableContext(s); err == nil {
		t.Fatal("context identity collision accepted")
	}
}
