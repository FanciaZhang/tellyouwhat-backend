package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func diagramRevisionFixture() (Revision, Snapshot) {
	id, block := uuid.NewString(), uuid.NewString()
	content, instruction := "周末去散步。", "整理成思维导图。"
	g := diagramFixture()
	sources := []TableSource{{SourceID: id, Anchor: TextAnchor{Quote: content}}}
	for i := range g.Nodes {
		g.Nodes[i].Sources = sources
	}
	for i := range g.Edges {
		g.Edges[i].Sources = sources
	}
	s := Snapshot{PendingUtterances: []SourceUtterance{{ID: id, Text: content + instruction}}}
	r := Revision{ConsumedSourceIDs: []string{id}, DiagramCreations: []DiagramCreation{{ID: uuid.NewString(), BlockID: block, SourceID: id, Instruction: instruction, Diagram: g}}, SourcePartitions: []SourcePartition{{SourceID: id, Segments: []SourceSegment{
		{Text: content, Role: "content", BlockIDs: []string{block}}, {Text: instruction, Role: "instruction", BlockIDs: []string{block}},
	}}}}
	return r, s
}

func TestDiagramRevisionAuthorizesProposalAndSourcePartitions(t *testing.T) {
	r, s := diagramRevisionFixture()
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	for name, mutate := range map[string]func(*Revision){
		"duplicate proposal": func(r *Revision) { r.DiagramCreations = append(r.DiagramCreations, r.DiagramCreations[0]) },
		"identity case collision": func(r *Revision) {
			r.DiagramCreations[0].Diagram.Nodes[0].ID = strings.ToUpper(r.DiagramCreations[0].ID)
		},
		"unknown destination": func(r *Revision) { id := uuid.NewString(); r.DiagramCreations[0].AfterID = &id },
		"instruction as content": func(r *Revision) {
			r.DiagramCreations[0].Diagram.Nodes[0].Sources[0].Anchor.Quote = r.DiagramCreations[0].Instruction
		},
		"unknown node source":   func(r *Revision) { r.DiagramCreations[0].Diagram.Nodes[0].Sources[0].SourceID = uuid.NewString() },
		"unknown edge source":   func(r *Revision) { r.DiagramCreations[0].Diagram.Edges[0].Sources[0].SourceID = uuid.NewString() },
		"missing partitions":    func(r *Revision) { r.SourcePartitions = nil },
		"instruction disguised": func(r *Revision) { r.SourcePartitions[0].Segments[1].Role = "content" },
		"unknown body":          func(r *Revision) { r.DiagramCreations[0].Diagram.Nodes[0].BlockIDs = []string{uuid.NewString()} },
	} {
		t.Run(name, func(t *testing.T) {
			var bad Revision
			if err := json.Unmarshal(data, &bad); err != nil {
				t.Fatal(err)
			}
			mutate(&bad)
			if bad.Validate(s) == nil {
				t.Fatal("unsafe proposal accepted")
			}
		})
	}
}

func TestDiagramRevisionUsesHistoricalContentWithoutReconsumingIt(t *testing.T) {
	r, s := diagramRevisionFixture()
	old := uuid.NewString()
	content := r.SourcePartitions[0].Segments[0].Text
	s.KnownSourceIDs = []string{old}
	s.DiagramSourceContext = []TableSource{{SourceID: old, Anchor: TextAnchor{Quote: content}}}
	s.PendingUtterances[0].Text = r.DiagramCreations[0].Instruction
	r.SourcePartitions[0].Segments = r.SourcePartitions[0].Segments[1:]
	for i := range r.DiagramCreations[0].Diagram.Nodes {
		r.DiagramCreations[0].Diagram.Nodes[i].Sources = []TableSource{{SourceID: old, Anchor: TextAnchor{Quote: content}}}
	}
	for i := range r.DiagramCreations[0].Diagram.Edges {
		r.DiagramCreations[0].Diagram.Edges[i].Sources = []TableSource{{SourceID: old, Anchor: TextAnchor{Quote: content}}}
	}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	s.DiagramSourceContext = nil
	if r.Validate(s) == nil {
		t.Fatal("missing historical evidence accepted")
	}
}
