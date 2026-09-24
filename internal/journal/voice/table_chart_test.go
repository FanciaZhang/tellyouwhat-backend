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
