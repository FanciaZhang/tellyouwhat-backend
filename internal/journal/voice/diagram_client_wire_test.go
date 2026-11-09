package voice

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Stable encoder output is also exercised by the Swift decoder acceptance test.
func TestDiagramEditClientWireFixture(t *testing.T) {
	id := func(n int) string { return fmt.Sprintf("20000000-0000-4000-8000-%012d", n) }
	title := "河岸漫步"
	c := DiagramEdit{ID: id(1), BlockID: id(2), DiagramID: id(3), SourceID: id(4), Instruction: "把散步改名为河岸漫步",
		Actions: []DiagramEditAction{{Update: &DiagramNodeUpdate{NodeID: id(6), Title: &title}}}}
	s := Snapshot{Blocks: []Block{{ID: id(2)}}, BlockComponents: map[string][]string{id(2): {id(3)}},
		PendingUtterances: []SourceUtterance{{ID: id(4), Text: c.Instruction}},
		DiagramContext: []DiagramContext{{BlockID: id(2), DiagramID: id(3), Title: "周末", Kind: "mindMap",
			Nodes: []DiagramContextNode{{ID: id(5), Title: "周末", BlockIDs: []string{}}, {ID: id(6), Title: "散步", BlockIDs: []string{}}},
			Edges: []DiagramContextEdge{{ID: id(7), From: id(5), To: id(6)}}}}}
	r := Revision{DiagramEdits: []DiagramEdit{c}, ConsumedSourceIDs: []string{id(4)},
		SourcePartitions: []SourcePartition{{SourceID: id(4), Segments: []SourceSegment{{Text: c.Instruction, Role: "instruction", BlockIDs: []string{id(2)}}}}}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("DIAGRAM_EDIT_CLIENT_WIRE=" + string(encoded))
}

func TestDiagramClientWireFixture(t *testing.T) {
	id := func(n int) string { return fmt.Sprintf("10000000-0000-4000-8000-%012d", n) }
	source := TableSource{SourceID: id(1), Anchor: TextAnchor{Quote: "周末去散步。"}}
	c := DiagramCreation{ID: id(2), BlockID: id(3), SourceID: id(1), Instruction: "整理成思维导图。", Diagram: DiagramGraph{
		ID: id(4), Title: "周末", Kind: "mindMap",
		Nodes: []DiagramNode{{ID: id(5), Title: "周末", BlockIDs: []string{}, Sources: []TableSource{source}}, {ID: id(6), Title: "散步", BlockIDs: []string{}, Sources: []TableSource{source}}},
		Edges: []DiagramEdge{{ID: id(7), From: id(5), To: id(6), Label: "活动", Sources: []TableSource{source}}},
	}}
	r := Revision{DiagramCreations: []DiagramCreation{c}, ConsumedSourceIDs: []string{id(1)}, SourcePartitions: []SourcePartition{{SourceID: id(1), Segments: []SourceSegment{
		{Text: source.Anchor.Quote, Role: "content", BlockIDs: []string{id(3)}}, {Text: c.Instruction, Role: "instruction", BlockIDs: []string{id(3)}},
	}}}, SemanticState: SemanticState{Entities: []Entity{}, UnresolvedMentions: []Mention{}, Outline: []OutlineElement{}}}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for name, raw := range voiceRevisionSchema()["properties"].(map[string]any) {
		if field, ok := raw.(map[string]any); ok && field["type"] == "array" && fields[name] == nil {
			fields[name] = []any{}
		}
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(normalized, &r); err != nil {
		t.Fatal(err)
	}
	s := Snapshot{PendingUtterances: []SourceUtterance{{ID: id(1), Text: source.Anchor.Quote + c.Instruction}}}
	if err = r.Validate(s); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("DIAGRAM_CLIENT_WIRE=" + string(encoded))
}
