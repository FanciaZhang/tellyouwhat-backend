package voice

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Diagram sources deliberately omit the client-owned recording archive ID.
type DiagramNode struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Detail      string        `json:"detail"`
	BlockIDs    []string      `json:"blockIDs"`
	Sources     []TableSource `json:"sources"`
	NeedsReview bool          `json:"needsReview"`
}
type DiagramEdge struct {
	ID          string        `json:"id"`
	From        string        `json:"from"`
	To          string        `json:"to"`
	Label       string        `json:"label"`
	Sources     []TableSource `json:"sources"`
	NeedsReview bool          `json:"needsReview"`
}
type DiagramGraph struct {
	ID    string        `json:"id"`
	Title string        `json:"title"`
	Kind  string        `json:"kind"`
	Nodes []DiagramNode `json:"nodes"`
	Edges []DiagramEdge `json:"edges"`
}
type DiagramCreation struct {
	ID          string       `json:"id"`
	BlockID     string       `json:"blockID"`
	AfterID     *string      `json:"afterID"`
	SourceID    string       `json:"sourceID"`
	Instruction string       `json:"instruction"`
	Diagram     DiagramGraph `json:"diagram"`
}

// Structural validity is only one gate. Callers must separately authorize every
// source against current content partitions and reserve document-wide IDs.
func validateDiagramGraph(g DiagramGraph) error {
	if !validID(g.ID) || strings.TrimSpace(g.Title) == "" || utf8.RuneCountInString(g.Title) > 300 ||
		len(g.Nodes) == 0 || len(g.Nodes) > 500 || len(g.Edges) > 2000 ||
		(g.Kind != "mindMap" && g.Kind != "relationship" && g.Kind != "flow") {
		return ErrInvalid
	}
	used := map[string]bool{strings.ToLower(g.ID): true}
	claim := func(id string) bool {
		key := strings.ToLower(id)
		if !validID(id) || used[key] {
			return false
		}
		used[key] = true
		return true
	}
	sourcesValid := func(sources []TableSource) bool {
		if len(sources) == 0 {
			return false
		}
		for _, s := range sources {
			if !validID(s.SourceID) || strings.TrimSpace(s.Anchor.Quote) == "" ||
				utf8.RuneCountInString(s.Anchor.Quote+s.Anchor.Prefix+s.Anchor.Suffix) > 6000 {
				return false
			}
		}
		return true
	}
	nodes, parents, children := map[string]bool{}, map[string]int{}, map[string][]string{}
	total := 0
	for _, n := range g.Nodes {
		if !claim(n.ID) || strings.TrimSpace(n.Title) == "" || utf8.RuneCountInString(n.Title) > 300 ||
			utf8.RuneCountInString(n.Detail) > 6000 || len(n.BlockIDs) > 64 || !sourcesValid(n.Sources) {
			return ErrInvalid
		}
		refs := map[string]bool{}
		for _, id := range n.BlockIDs {
			key := strings.ToLower(id)
			if !validID(id) || refs[key] {
				return ErrInvalid
			}
			refs[key] = true
		}
		total += len(utf16.Encode([]rune(n.Title + n.Detail)))
		if total > 200000 {
			return ErrInvalid
		}
		nodes[strings.ToLower(n.ID)] = true
	}
	for _, e := range g.Edges {
		from, to := strings.ToLower(e.From), strings.ToLower(e.To)
		if !claim(e.ID) || !nodes[from] || !nodes[to] || from == to || utf8.RuneCountInString(e.Label) > 300 || !sourcesValid(e.Sources) {
			return ErrInvalid
		}
		parents[to]++
		children[from] = append(children[from], to)
	}
	if g.Kind != "mindMap" {
		return nil
	} // Flows and relationships may contain cycles.
	if len(g.Edges) != len(g.Nodes)-1 {
		return ErrInvalid
	}
	roots := []string{}
	for id := range nodes {
		if parents[id] > 1 {
			return ErrInvalid
		}
		if parents[id] == 0 {
			roots = append(roots, id)
		}
	}
	if len(roots) != 1 {
		return ErrInvalid
	}
	seen, pending := map[string]bool{}, roots
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[next] {
			return ErrInvalid
		}
		seen[next] = true
		pending = append(pending, children[next]...)
	}
	if len(seen) != len(nodes) {
		return ErrInvalid
	}
	return nil
}
