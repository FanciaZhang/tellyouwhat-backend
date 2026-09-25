package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"reflect"
	"strings"
	"testing"
)

func diagramContextFixture() Snapshot {
	s := journeyContextFixture()
	g := diagramFixture()
	c := DiagramContext{BlockID: s.Blocks[0].ID, DiagramID: g.ID, Title: g.Title, Kind: g.Kind}
	for _, n := range g.Nodes {
		c.Nodes = append(c.Nodes, DiagramContextNode{ID: n.ID, Title: n.Title, BlockIDs: []string{s.Blocks[0].ID}})
	}
	for _, e := range g.Edges {
		c.Edges = append(c.Edges, DiagramContextEdge{ID: e.ID, From: e.From, To: e.To, Label: e.Label})
	}
	s.DiagramContext = []DiagramContext{c}
	s.BlockComponents[c.BlockID] = append(s.BlockComponents[c.BlockID], c.DiagramID)
	return s
}

func TestDiagramContextModelProjection(t *testing.T) {
	s := diagramContextFixture()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var model rewriteModelDocument
	if err := json.Unmarshal(data, &model); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(model.DiagramContext, s.DiagramContext) {
		t.Fatal("graph identity, order or references changed")
	}
	projection, _ := json.Marshal(model.DiagramContext)
	for _, forbidden := range []string{"archiveID", "sourceID", "anchor", "sources"} {
		if strings.Contains(string(projection), forbidden) {
			t.Fatal("private evidence in structural projection", forbidden)
		}
	}
}

func TestDiagramContextRejectsAmbiguousStructure(t *testing.T) {
	cases := map[string]func(*Snapshot){
		"unknown owner":           func(s *Snapshot) { s.DiagramContext[0].BlockID = uuid.NewString() },
		"unknown graph":           func(s *Snapshot) { s.DiagramContext[0].DiagramID = uuid.NewString() },
		"duplicate owner":         func(s *Snapshot) { s.BlockComponents[uuid.NewString()] = []string{s.DiagramContext[0].DiagramID} },
		"duplicate graph":         func(s *Snapshot) { s.DiagramContext = append(s.DiagramContext, s.DiagramContext[0]) },
		"cross type graph":        func(s *Snapshot) { s.DiagramContext[0].DiagramID = s.JourneyContext[0].MapID },
		"timeline node collision": func(s *Snapshot) { s.DiagramContext[0].Nodes[0].ID = s.TimelineContext[0].Events[0].ID },
		"table node collision":    func(s *Snapshot) { s.DiagramContext[0].Nodes[0].ID = s.TableContext[0].Columns[0].ID },
		"case collision":          func(s *Snapshot) { s.DiagramContext[0].Nodes[1].ID = strings.ToUpper(s.DiagramContext[0].Nodes[0].ID) },
		"unknown body reference":  func(s *Snapshot) { s.DiagramContext[0].Nodes[0].BlockIDs = []string{uuid.NewString()} },
		"disconnected tree":       func(s *Snapshot) { s.DiagramContext[0].Edges = nil },
		"too many":                func(s *Snapshot) { s.DiagramContext = make([]DiagramContext, 5) },
		"aggregate budget": func(s *Snapshot) {
			for i := 0; i < 2; i++ {
				s.DiagramContext[0].Nodes[i].Detail = strings.Repeat("文", 6000)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := diagramContextFixture()
			change(&s)
			if s.Validate() == nil {
				t.Fatal("invalid graph context accepted")
			}
		})
	}
}
