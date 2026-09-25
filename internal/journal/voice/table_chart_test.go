package voice

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
)

func TestChartModelTransportRoundTrip(t *testing.T) {
	s, r := tableEditFixture()
	table := s.TableContext[0]
	instruction := "把价格画成柱状图"
	s.PendingUtterances[0].Text = instruction
	r.TableEdits[0].Instruction = instruction
	r.SourcePartitions[0].Segments[0].Text = instruction
	chart := TableChart{ID: uuid.NewString(), Title: "价格比较", Kind: "bar", CategoryColumnID: table.Columns[0].ID, ValueColumnIDs: []string{table.Columns[1].ID}}
	r.TableEdits[0].Patches = []TablePatch{{Kind: "addChart", TargetID: table.TableID, Chart: &chart}}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"diagramEdits", "diagramCreations", "journeyEdits", "journeyCreations", "timelineEdits", "timelineCreations", "tableCreations", "tableResolutions", "formatCommands", "moveCommands", "paragraphCommands", "paragraphResolutions", "moveResolutions", "formatResolutions"} {
		fields[key] = []any{}
	}
	response, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Input string `json:"input"`
			Store bool   `json:"store"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		var input rewriteModelDocument
		if err := json.Unmarshal([]byte(body.Input), &input); err != nil {
			t.Error(err)
		}
		if body.Store || len(input.TableContext) != 1 || !reflect.DeepEqual(input.TableContext[0].Columns, table.Columns) {
			t.Error("table projection changed")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(response)}}}}})
	}))
	defer server.Close()
	result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "fixture", Model: "fixture", HTTP: server.Client()}).Rewrite(context.Background(), s, 0)
	if err != nil || !reflect.DeepEqual(result.Revision.TableEdits, r.TableEdits) {
		t.Fatal("chart transport failed", err)
	}
}

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

func TestChartModelSchemaIsStrictAndReferenceOnly(t *testing.T) {
	schema := voiceRevisionSchema()
	edit := schema["properties"].(map[string]any)["tableEdits"].(map[string]any)["items"].(map[string]any)
	patch := edit["properties"].(map[string]any)["patches"].(map[string]any)["items"].(map[string]any)
	if !slices.Contains(patch["required"].([]string), "chart") || patch["additionalProperties"] != false {
		t.Fatal("chart payload not strict")
	}
	properties := patch["properties"].(map[string]any)
	kinds := properties["kind"].(map[string]any)["enum"].([]string)
	for _, kind := range []string{"addChart", "replaceChart", "removeChart"} {
		if !slices.Contains(kinds, kind) {
			t.Fatal("missing operation", kind)
		}
	}
	chart := properties["chart"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
	if chart["additionalProperties"] != false || len(chart["required"].([]string)) != 5 {
		t.Fatal("chart schema accepts unbounded payload")
	}
	fields := chart["properties"].(map[string]any)
	for _, key := range []string{"id", "title", "kind", "categoryColumnID", "valueColumnIDs"} {
		if fields[key] == nil || !slices.Contains(chart["required"].([]string), key) {
			t.Fatal("missing chart field", key)
		}
	}
	if fields["values"] != nil || fields["sources"] != nil {
		t.Fatal("duplicate values or evidence in chart schema")
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
