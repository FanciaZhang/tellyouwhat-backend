package voice

import (
	"encoding/json"
	"strings"
)

// Current structure only. Original speech belongs to separately bounded source
// context, never implicitly to the graph projection.
type DiagramContextNode struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Detail      string   `json:"detail"`
	BlockIDs    []string `json:"blockIDs"`
	NeedsReview bool     `json:"needsReview"`
}
type DiagramContextEdge struct {
	ID          string `json:"id"`
	From        string `json:"from"`
	To          string `json:"to"`
	Label       string `json:"label"`
	NeedsReview bool   `json:"needsReview"`
}
type DiagramContext struct {
	BlockID   string               `json:"blockID"`
	DiagramID string               `json:"diagramID"`
	Title     string               `json:"title"`
	Kind      string               `json:"kind"`
	Nodes     []DiagramContextNode `json:"nodes"`
	Edges     []DiagramContextEdge `json:"edges"`
}

func validateDiagramContext(s Snapshot) error {
	if len(s.DiagramContext) > 4 {
		return ErrInvalid
	}
	data, err := json.Marshal(s.DiagramContext)
	if err != nil || len(data) > 32000 {
		return ErrInvalid
	}
	key := strings.ToLower
	used, blocks, owners := map[string]bool{}, map[string]bool{}, map[string][]string{}
	for _, b := range s.Blocks {
		used[key(b.ID)] = true
		blocks[key(b.ID)] = true
	}
	for owner, ids := range s.BlockComponents {
		for _, id := range ids {
			used[key(id)] = true
			owners[key(id)] = append(owners[key(id)], key(owner))
		}
	}
	otherGraphs := map[string]bool{}
	reserve := func(id string) { used[key(id)] = true; otherGraphs[key(id)] = true }
	for _, t := range s.TimelineContext {
		otherGraphs[key(t.TimelineID)] = true
		for _, e := range t.Events {
			reserve(e.ID)
		}
	}
	for _, m := range s.JourneyContext {
		otherGraphs[key(m.MapID)] = true
		for _, stop := range m.Stops {
			reserve(stop.ID)
		}
	}
	for _, t := range s.TableContext {
		otherGraphs[key(t.TableID)] = true
		for _, row := range t.Rows {
			reserve(row.ID)
		}
		for _, col := range t.Columns {
			reserve(col.ID)
		}
		for _, c := range t.Calculations {
			reserve(c.ID)
		}
		for _, c := range t.Charts {
			reserve(c.ID)
		}
	}
	for _, c := range s.FormatContext {
		reserve(c.ReceiptID)
	}
	for _, c := range s.MoveContext {
		reserve(c.ReceiptID)
	}
	for _, c := range s.ParagraphContext {
		reserve(c.ReceiptID)
	}
	for _, c := range s.TableReceiptContext {
		reserve(c.ReceiptID)
	}
	seen := map[string]bool{}
	for _, c := range s.DiagramContext {
		id, owner := key(c.DiagramID), key(c.BlockID)
		if !blocks[owner] || blocks[id] || otherGraphs[id] || seen[id] || len(owners[id]) != 1 || owners[id][0] != owner {
			return ErrInvalid
		}
		seen[id] = true
		g := DiagramGraph{ID: c.DiagramID, Title: c.Title, Kind: c.Kind}
		for _, n := range c.Nodes {
			if used[key(n.ID)] {
				return ErrInvalid
			}
			used[key(n.ID)] = true
			for _, ref := range n.BlockIDs {
				if !blocks[key(ref)] {
					return ErrInvalid
				}
			}
			g.Nodes = append(g.Nodes, DiagramNode{ID: n.ID, Title: n.Title, Detail: n.Detail, BlockIDs: n.BlockIDs, NeedsReview: n.NeedsReview})
		}
		for _, e := range c.Edges {
			if used[key(e.ID)] {
				return ErrInvalid
			}
			used[key(e.ID)] = true
			g.Edges = append(g.Edges, DiagramEdge{ID: e.ID, From: e.From, To: e.To, Label: e.Label, NeedsReview: e.NeedsReview})
		}
		if validateDiagramShape(g, false) != nil {
			return ErrInvalid
		}
	}
	return nil
}
