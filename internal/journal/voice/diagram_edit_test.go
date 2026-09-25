package voice

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestDiagramEditProjectsSequentialStructureWithoutMutatingContext(t *testing.T) {
	root, a, b, leaf := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	ea, eb, el := uuid.NewString(), uuid.NewString(), uuid.NewString()
	context := DiagramContext{BlockID: uuid.NewString(), DiagramID: uuid.NewString(), Title: "计划", Kind: "mindMap",
		Nodes: []DiagramContextNode{{ID: root, Title: "周末"}, {ID: a, Title: "散步"}, {ID: b, Title: "摄影"}, {ID: leaf, Title: "河边"}},
		Edges: []DiagramContextEdge{{ID: ea, From: root, To: a}, {ID: eb, From: root, To: b}, {ID: el, From: a, To: leaf}},
	}
	before, _ := json.Marshal(context)
	command := DiagramEdit{ID: uuid.NewString(), BlockID: context.BlockID, DiagramID: context.DiagramID}
	title := "河岸漫步"
	command.Actions = []DiagramEditAction{{Move: &DiagramNodeMove{NodeID: a, ParentID: b}}, {Update: &DiagramNodeUpdate{NodeID: a, Title: &title}}}
	result, err := command.project(context)
	if err != nil || result.Edges[0].From != b || result.Nodes[1].Title != title || result.Edges[2].From != a {
		t.Fatalf("sequential projection: %+v %v", result, err)
	}
	for name, actions := range map[string][]DiagramEditAction{
		"cycle":            {{Move: &DiagramNodeMove{NodeID: a, ParentID: leaf}}},
		"root":             {{Remove: &DiagramNodeRemoval{NodeID: root}}},
		"unknown":          {{Update: &DiagramNodeUpdate{NodeID: uuid.NewString(), Title: &title}}},
		"multiple cases":   {{Move: &DiagramNodeMove{NodeID: a, ParentID: b}, Remove: &DiagramNodeRemoval{NodeID: a}}},
		"empty":            {{}},
		"duplicate order":  {{Order: &DiagramChildOrder{ParentID: root, EdgeIDs: []string{ea, ea}}}},
		"missing order":    {{Order: &DiagramChildOrder{ParentID: root, EdgeIDs: []string{ea}}}},
		"reuse deleted":    {{Remove: &DiagramNodeRemoval{NodeID: a}}, {Add: &DiagramNodeAddition{NodeID: leaf, EdgeID: uuid.NewString(), ParentID: root, Title: title}}},
		"merge descendant": {{Merge: &DiagramNodeMerge{NodeID: a, TargetID: leaf, Title: title}}},
		"no change":        {{Order: &DiagramChildOrder{ParentID: root, EdgeIDs: []string{ea, eb}}}},
	} {
		t.Run(name, func(t *testing.T) {
			c := command
			c.Actions = actions
			if _, err := c.project(context); err == nil {
				t.Fatal("accepted invalid edit")
			}
		})
	}
	command.Actions = []DiagramEditAction{{Merge: &DiagramNodeMerge{NodeID: a, TargetID: b, Title: "散步摄影"}}}
	merged, err := command.project(context)
	if err != nil || len(merged.Nodes) != 3 || merged.Edges[1].From != b {
		t.Fatalf("merge: %+v %v", merged, err)
	}
	command.Actions = []DiagramEditAction{{Order: &DiagramChildOrder{ParentID: root, EdgeIDs: []string{eb, ea}}}}
	ordered, err := command.project(context)
	if err != nil || ordered.Edges[0].ID != eb || ordered.Edges[2].ID != el {
		t.Fatalf("order: %+v %v", ordered, err)
	}
	command.Actions = []DiagramEditAction{{Remove: &DiagramNodeRemoval{NodeID: a}}, {Add: &DiagramNodeAddition{NodeID: uuid.NewString(), EdgeID: uuid.NewString(), ParentID: root, Title: "午餐"}}}
	added, err := command.project(context)
	if err != nil || len(added.Nodes) != 3 {
		t.Fatalf("remove descendants and add: %+v %v", added, err)
	}
	after, _ := json.Marshal(context)
	if string(before) != string(after) {
		t.Fatal("projection mutated input context")
	}
}

func TestDiagramEditActionWireShapes(t *testing.T) {
	title := "河岸漫步"
	actions := []DiagramEditAction{
		{Update: &DiagramNodeUpdate{NodeID: "node", Title: &title}},
		{Move: &DiagramNodeMove{NodeID: "node", ParentID: "parent"}},
		{Merge: &DiagramNodeMerge{NodeID: "node", TargetID: "target", Title: title}},
		{Remove: &DiagramNodeRemoval{NodeID: "node"}},
		{Add: &DiagramNodeAddition{NodeID: "new", EdgeID: "edge", ParentID: "parent", Title: title}},
		{Order: &DiagramChildOrder{ParentID: "parent", EdgeIDs: []string{"edge"}}},
	}
	for i, action := range actions {
		data, err := json.Marshal(action)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 1 {
			t.Fatalf("action %d must encode exactly one case: %s", i, data)
		}
		var decoded DiagramEditAction
		if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(decoded, action) {
			t.Fatalf("action %d round trip: %v", i, err)
		}
	}
	data, _ := json.Marshal(actions[0])
	if string(data) != `{"update":{"nodeID":"node","title":"河岸漫步","detail":null}}` {
		t.Fatalf("nullable update contract changed: %s", data)
	}
	properties := diagramEditSchema()["properties"].(map[string]any)
	items := properties["actions"].(map[string]any)["items"].(map[string]any)
	if len(items["anyOf"].([]any)) != len(actions) {
		t.Fatal("missing action schema")
	}
}

func diagramEditRevisionFixture() (Revision, Snapshot) {
	s := diagramContextFixture()
	context := s.DiagramContext[0]
	source := uuid.NewString()
	instruction, title := "把第二个主题改名为河岸漫步", "河岸漫步"
	s.PendingUtterances = []SourceUtterance{{ID: source, Text: instruction}}
	r := Revision{ConsumedSourceIDs: []string{source}, DiagramEdits: []DiagramEdit{{ID: uuid.NewString(),
		BlockID: context.BlockID, DiagramID: context.DiagramID, SourceID: source, Instruction: instruction,
		Actions: []DiagramEditAction{{Update: &DiagramNodeUpdate{NodeID: context.Nodes[1].ID, Title: &title}}}}},
		SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: instruction, Role: "instruction", BlockIDs: []string{context.BlockID}}}}}}
	return r, s
}

func TestDiagramEditRevisionAuthorizesOnlyCurrentInstruction(t *testing.T) {
	r, s := diagramEditRevisionFixture()
	context, title := s.DiagramContext[0], "河岸漫步"
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	for name, change := range map[string]func(*Revision){
		"missing partition":       func(r *Revision) { r.SourcePartitions = nil },
		"content not instruction": func(r *Revision) { r.SourcePartitions[0].Segments[0].Role = "content" },
		"wrong target":            func(r *Revision) { r.SourcePartitions[0].Segments[0].BlockIDs = []string{uuid.NewString()} },
		"unconsumed":              func(r *Revision) { r.ConsumedSourceIDs = nil },
		"invented instruction":    func(r *Revision) { r.DiagramEdits[0].Instruction = "没有说过" },
		"unknown graph":           func(r *Revision) { r.DiagramEdits[0].DiagramID = uuid.NewString() },
		"duplicate target": func(r *Revision) {
			c := r.DiagramEdits[0]
			c.ID = uuid.NewString()
			r.DiagramEdits = append(r.DiagramEdits, c)
		},
		"existing command identity": func(r *Revision) { r.DiagramEdits[0].ID = context.Nodes[0].ID },
		"cross component new identity": func(r *Revision) {
			r.DiagramEdits[0].Actions = []DiagramEditAction{{Add: &DiagramNodeAddition{NodeID: s.JourneyContext[0].MapID, EdgeID: uuid.NewString(), ParentID: context.Nodes[0].ID, Title: title}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var changed Revision
			if err := json.Unmarshal(data, &changed); err != nil {
				t.Fatal(err)
			}
			change(&changed)
			if err := changed.Validate(s); err == nil {
				t.Fatal("unauthorized edit accepted")
			}
		})
	}
}
