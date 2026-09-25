package voice

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

func (g DiagramGraph) sources() []TableSource {
	result := []TableSource{}
	for _, n := range g.Nodes {
		result = append(result, n.Sources...)
	}
	for _, e := range g.Edges {
		result = append(result, e.Sources...)
	}
	return result
}

func validateDiagramCreations(r Revision, s Snapshot) error {
	if len(r.DiagramCreations) == 0 {
		return nil
	}
	data, err := json.Marshal(r.DiagramCreations)
	if err != nil || len(data) > 64000 || len(r.DiagramCreations) > 4 || len(r.MoveCommands)+len(r.MoveResolutions)+len(r.ParagraphCommands)+len(r.ParagraphResolutions) > 0 {
		return ErrInvalid
	}
	if validateStructuredSourceContext(s.DiagramSourceContext, s) != nil || validateDiagramContext(s) != nil {
		return ErrInvalid
	}
	used, blocks := map[string]bool{}, map[string]bool{}
	reserve := func(ids ...string) {
		for _, id := range ids {
			used[strings.ToLower(id)] = true
		}
	}
	for _, b := range s.Blocks {
		reserve(b.ID)
		blocks[strings.ToLower(b.ID)] = true
	}
	for _, ids := range s.BlockComponents {
		reserve(ids...)
	}
	for _, c := range s.DiagramContext {
		reserve(c.DiagramID)
		for _, n := range c.Nodes {
			reserve(n.ID)
		}
		for _, e := range c.Edges {
			reserve(e.ID)
		}
	}
	for _, c := range s.TimelineContext {
		reserve(c.TimelineID)
		for _, e := range c.Events {
			reserve(e.ID)
		}
	}
	for _, c := range s.JourneyContext {
		reserve(c.MapID)
		for _, e := range c.Stops {
			reserve(e.ID)
		}
	}
	for _, c := range s.TableContext {
		reserve(c.TableID)
		for _, e := range c.Rows {
			reserve(e.ID)
		}
		for _, e := range c.Columns {
			reserve(e.ID)
		}
		for _, e := range c.Calculations {
			reserve(e.ID)
		}
		for _, e := range c.Charts {
			reserve(e.ID)
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
	for _, c := range r.BlockEdits {
		reserve(c.ID)
	}
	for _, c := range r.FormatCommands {
		reserve(c.ID)
	}
	for _, c := range r.FormatResolutions {
		reserve(c.ID)
	}
	for _, c := range r.TableResolutions {
		reserve(c.ID)
	}
	for _, c := range r.TimelineCreations {
		reserve(c.ID, c.BlockID, c.TimelineID)
		for _, e := range c.Events {
			reserve(e.ID)
		}
	}
	for _, c := range r.TimelineEdits {
		reserve(c.ID)
		for _, e := range c.Insertions {
			reserve(e.ID)
		}
	}
	for _, c := range r.JourneyCreations {
		reserve(c.ID, c.BlockID, c.MapID)
		for _, e := range c.Visits {
			reserve(e.ID)
		}
	}
	for _, c := range r.JourneyEdits {
		reserve(c.ID)
		for _, e := range c.Insertions {
			reserve(e.ID)
		}
	}
	for _, c := range r.TableCreations {
		reserve(c.ID, c.BlockID, c.TableID)
		for _, e := range c.Rows {
			reserve(e.ID)
		}
		for _, e := range c.Columns {
			reserve(e.ID)
		}
	}
	for _, c := range r.TableEdits {
		reserve(c.ID)
		for _, p := range c.Patches {
			if p.Row != nil {
				reserve(p.Row.ID)
			}
			if p.Column != nil {
				reserve(p.Column.ID)
			}
			if p.Calculation != nil {
				reserve(p.Calculation.ID)
			}
		}
	}
	claim := func(id string) bool {
		key := strings.ToLower(id)
		if !validID(id) || used[key] {
			return false
		}
		used[key] = true
		return true
	}
	for _, c := range r.DiagramCreations {
		if !claim(c.ID) || !claim(c.BlockID) || !claim(c.Diagram.ID) || validateDiagramGraph(c.Diagram) != nil ||
			c.AfterID != nil && !blocks[strings.ToLower(*c.AfterID)] || utf8.RuneCountInString(c.Instruction) > 500 ||
			!diagramSourceAuthorized(TableSource{SourceID: c.SourceID, Anchor: TextAnchor{Quote: c.Instruction}}, "instruction", c.BlockID, r, s, nil) {
			return ErrInvalid
		}
		for _, n := range c.Diagram.Nodes {
			if !claim(n.ID) {
				return ErrInvalid
			}
			for _, ref := range n.BlockIDs {
				if !blocks[strings.ToLower(ref)] {
					return ErrInvalid
				}
			}
		}
		for _, e := range c.Diagram.Edges {
			if !claim(e.ID) {
				return ErrInvalid
			}
		}
		for _, source := range c.Diagram.sources() {
			if !diagramSourceAuthorized(source, "content", c.BlockID, r, s, s.DiagramSourceContext) {
				return ErrInvalid
			}
		}
	}
	return nil
}
