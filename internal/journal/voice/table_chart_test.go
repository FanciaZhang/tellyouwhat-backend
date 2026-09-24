package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"reflect"
	"testing"
)

func TestChartReferencesValidateShapeAndDataWithoutInventingValues(t *testing.T) {
	table := tableContextFixture().TableContext[0]
	chart := TableChart{ID: uuid.NewString(), Title: "费用", Kind: "bar", CategoryColumnID: table.Columns[0].ID, ValueColumnIDs: []string{table.Columns[1].ID}}
	if !chart.canResolve(table) {
		t.Fatal("valid chart rejected")
	}
	data, err := json.Marshal(chart)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TableChart
	if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(chart, decoded) {
		t.Fatal("round trip failed", err)
	}
	chart.Kind = "proportion"
	if !chart.canResolve(table) {
		t.Fatal("positive proportion rejected")
	}
	table.Rows[0].Cells[1].NeedsReview = true
	if chart.canResolve(table) {
		t.Fatal("review data accepted as complete proportion")
	}
	chart.Kind = "line"
	if !chart.canResolve(table) {
		t.Fatal("line gap rejected")
	}
	table.Rows[0].Cells[1].NeedsReview = false
	table.Rows[0].Cells[1].Number = "-1"
	chart.Kind = "proportion"
	if chart.canResolve(table) {
		t.Fatal("negative proportion accepted")
	}
	chart.Kind = "bar"
	chart.ValueColumnIDs = []string{uuid.NewString()}
	if !chart.validShape() || chart.canResolve(table) {
		t.Fatal("dangling reference policy incorrect")
	}
	chart.ValueColumnIDs = []string{chart.CategoryColumnID}
	if chart.validShape() {
		t.Fatal("category reused as value")
	}
}

func TestChartPatchesAndRevisionRequireInstructionAndPreserveSource(t *testing.T) {
	s, r := tableEditFixture()
	before := s.TableContext[0]
	chart := TableChart{ID: uuid.NewString(), Title: "费用图", Kind: "bar", CategoryColumnID: before.Columns[0].ID, ValueColumnIDs: []string{before.Columns[1].ID}}
	patch := TablePatch{Kind: "addChart", TargetID: before.TableID, Chart: &chart}
	r.TableEdits[0].Patches = []TablePatch{patch}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	r.SourcePartitions[0].Segments[0].Role = "content"
	if err := r.Validate(s); err == nil {
		t.Fatal("missing instruction authorized")
	}
	r.SourcePartitions[0].Segments[0].Role = "instruction"
	after, err := applyTablePatches(before, []TablePatch{patch})
	if err != nil || !reflect.DeepEqual(after.Rows, before.Rows) || len(before.Charts) != 0 {
		t.Fatal("source mutation", err)
	}
	if len(after.Charts) != 1 {
		t.Fatal("chart missing")
	}
	if _, err := applyTablePatches(after, []TablePatch{patch}); err == nil {
		t.Fatal("duplicate ID accepted")
	}
	patch.Kind, patch.TargetID, chart.Kind = "replaceChart", chart.ID, "line"
	replaced, err := applyTablePatches(after, []TablePatch{patch})
	if err != nil || replaced.Charts[0].Kind != "line" || after.Charts[0].Kind != "bar" {
		t.Fatal("replacement aliased original", err)
	}
	removed, err := applyTablePatches(replaced, []TablePatch{{Kind: "removeChart", TargetID: chart.ID}})
	if err != nil || len(removed.Charts) != 0 || len(replaced.Charts) != 1 {
		t.Fatal("removal altered input", err)
	}
	s.TableContext[0] = after
	if err := r.Validate(s); err == nil {
		t.Fatal("reused ID authorized")
	}
	s.TableContext[0].Charts[0].ValueColumnIDs = []string{uuid.NewString()}
	if err := validateTableContext(s); err != nil {
		t.Fatal("dangling historical chart rejected", err)
	}
	s.TableContext[0].Charts[0].ID = before.Rows[0].ID
	if err := validateTableContext(s); err == nil {
		t.Fatal("context collision accepted")
	}
}
