package voice

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func diagramFixture() DiagramGraph {
	a, b := uuid.NewString(), uuid.NewString()
	sources := []TableSource{{SourceID: uuid.NewString(), Anchor: TextAnchor{Quote: "周末去散步"}}}
	return DiagramGraph{ID: uuid.NewString(), Title: "周末", Kind: "mindMap",
		Nodes: []DiagramNode{{ID: a, Title: "周末", Sources: sources}, {ID: b, Title: "散步", Sources: sources}},
		Edges: []DiagramEdge{{ID: uuid.NewString(), From: a, To: b, Sources: sources}}}
}

func TestDiagramGraphStructureAndWire(t *testing.T) {
	g := diagramFixture()
	if err := validateDiagramGraph(g); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "archiveID") {
		t.Fatal("local archive identity leaked into wire model")
	}
	var decoded DiagramGraph
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := validateDiagramGraph(decoded); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"flow", "relationship"} {
		g := diagramFixture()
		g.Kind = kind
		g.Edges = append(g.Edges, DiagramEdge{ID: uuid.NewString(), From: g.Nodes[1].ID, To: g.Nodes[0].ID, Sources: g.Nodes[0].Sources})
		if err := validateDiagramGraph(g); err != nil {
			t.Fatalf("valid cyclic %s: %v", kind, err)
		}
	}
}

func TestDiagramGraphRejectsInvalidShapes(t *testing.T) {
	cases := map[string]func(*DiagramGraph){
		"unknown kind":                 func(g *DiagramGraph) { g.Kind = "picture" },
		"empty":                        func(g *DiagramGraph) { g.Nodes = nil },
		"blank title":                  func(g *DiagramGraph) { g.Title = "  " },
		"duplicate node ignoring case": func(g *DiagramGraph) { g.Nodes[1].ID = strings.ToUpper(g.Nodes[0].ID) },
		"graph node collision":         func(g *DiagramGraph) { g.Nodes[0].ID = g.ID },
		"edge node collision":          func(g *DiagramGraph) { g.Edges[0].ID = g.Nodes[0].ID },
		"dangling edge":                func(g *DiagramGraph) { g.Edges[0].To = uuid.NewString() },
		"self edge":                    func(g *DiagramGraph) { g.Edges[0].To = g.Edges[0].From },
		"missing node evidence":        func(g *DiagramGraph) { g.Nodes[0].Sources = nil },
		"missing edge evidence":        func(g *DiagramGraph) { g.Edges[0].Sources = nil },
		"blank evidence":               func(g *DiagramGraph) { g.Nodes[0].Sources[0].Anchor.Quote = " " },
		"duplicate body refs":          func(g *DiagramGraph) { id := uuid.NewString(); g.Nodes[0].BlockIDs = []string{id, strings.ToUpper(id)} },
		"multiple roots":               func(g *DiagramGraph) { g.Edges = nil },
		"mind map cycle": func(g *DiagramGraph) {
			g.Edges = append(g.Edges, DiagramEdge{ID: uuid.NewString(), From: g.Nodes[1].ID, To: g.Nodes[0].ID, Sources: g.Nodes[0].Sources})
		},
		"oversized detail": func(g *DiagramGraph) { g.Nodes[0].Detail = strings.Repeat("文", 6001) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			g := diagramFixture()
			change(&g)
			if validateDiagramGraph(g) == nil {
				t.Fatal("invalid graph accepted")
			}
		})
	}
}
