package voice

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

func validateDiagramEdits(r Revision, s Snapshot) error {
	if len(r.DiagramEdits) == 0 {
		return nil
	}
	encoded, err := json.Marshal(r.DiagramEdits)
	if err != nil || len(encoded) > 64000 || len(r.DiagramEdits) > 4 ||
		len(r.MoveCommands)+len(r.MoveResolutions)+len(r.ParagraphCommands)+len(r.ParagraphResolutions) != 0 || validateDiagramContext(s) != nil {
		return ErrInvalid
	}
	// Reserve UUIDs in the snapshot and every other operation, including source
	// and receipt identities. New graph identities must not alias any of them.
	other := r
	other.DiagramEdits = nil
	data, err := json.Marshal([]any{s, other})
	if err != nil {
		return ErrInvalid
	}
	var tree any
	if json.Unmarshal(data, &tree) != nil {
		return ErrInvalid
	}
	used := map[string]bool{}
	var reserve func(any)
	reserve = func(value any) {
		switch v := value.(type) {
		case string:
			if validID(v) {
				used[strings.ToLower(v)] = true
			}
		case []any:
			for _, item := range v {
				reserve(item)
			}
		case map[string]any:
			for key, item := range v {
				reserve(key)
				reserve(item)
			}
		}
	}
	reserve(tree)
	claim := func(id string) bool {
		key := strings.ToLower(id)
		if !validID(id) || used[key] {
			return false
		}
		used[key] = true
		return true
	}
	touched := map[string]bool{}
	for _, c := range r.BlockEdits {
		touched[strings.ToLower(c.ID)] = true
	}
	for _, c := range r.Corrections {
		touched[strings.ToLower(c.BlockID)] = true
	}
	for _, c := range r.FormatCommands {
		touched[strings.ToLower(c.BlockID)] = true
	}
	for _, c := range r.TableEdits {
		touched[strings.ToLower(c.BlockID)] = true
	}
	for _, c := range r.TimelineEdits {
		touched[strings.ToLower(c.BlockID)] = true
	}
	for _, c := range r.JourneyEdits {
		touched[strings.ToLower(c.BlockID)] = true
	}
	for _, c := range r.FormatResolutions {
		for _, v := range s.FormatContext {
			if strings.EqualFold(c.ReceiptID, v.ReceiptID) {
				touched[strings.ToLower(v.BlockID)] = true
			}
		}
	}
	for _, c := range r.TableResolutions {
		for _, v := range s.TableReceiptContext {
			if strings.EqualFold(c.ReceiptID, v.ReceiptID) {
				touched[strings.ToLower(v.BlockID)] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range r.DiagramEdits {
		key := strings.ToLower(c.DiagramID)
		if !claim(c.ID) || seen[key] || touched[strings.ToLower(c.BlockID)] ||
			!slices.Contains(r.ConsumedSourceIDs, c.SourceID) || strings.TrimSpace(c.Instruction) == "" || utf8.RuneCountInString(c.Instruction) > 500 {
			return ErrInvalid
		}
		seen[key] = true
		source, matches := "", 0
		for _, u := range s.PendingUtterances {
			if u.ID == c.SourceID {
				source = u.Text
				matches++
			}
		}
		if matches != 1 || strings.Count(source, c.Instruction) != 1 {
			return ErrInvalid
		}
		parts, authorized := 0, false
		for _, p := range r.SourcePartitions {
			if p.SourceID != c.SourceID {
				continue
			}
			parts++
			var text strings.Builder
			for _, segment := range p.Segments {
				text.WriteString(segment.Text)
				if segment.Role == "instruction" && segment.Text == c.Instruction && slices.Equal(segment.BlockIDs, []string{c.BlockID}) {
					authorized = true
				}
			}
			if text.String() != source {
				return ErrInvalid
			}
		}
		if parts != 1 || !authorized {
			return ErrInvalid
		}
		contexts := 0
		for _, context := range s.DiagramContext {
			if !strings.EqualFold(context.DiagramID, c.DiagramID) {
				continue
			}
			contexts++
			if _, err := c.project(context); err != nil {
				return err
			}
		}
		if contexts != 1 {
			return ErrInvalid
		}
		for _, action := range c.Actions {
			if action.Add != nil && (!claim(action.Add.NodeID) || !claim(action.Add.EdgeID)) {
				return ErrInvalid
			}
		}
	}
	return nil
}

// Validate the entire sequence against a private projection. The caller still
// owns instruction authorization, document-wide identity and conflict checks.
func (c DiagramEdit) project(context DiagramContext) (DiagramGraph, error) {
	g := DiagramGraph{ID: context.DiagramID, Title: context.Title, Kind: context.Kind}
	for _, n := range context.Nodes {
		g.Nodes = append(g.Nodes, DiagramNode{ID: n.ID, Title: n.Title, Detail: n.Detail, BlockIDs: slices.Clone(n.BlockIDs), NeedsReview: n.NeedsReview})
	}
	for _, e := range context.Edges {
		g.Edges = append(g.Edges, DiagramEdge{ID: e.ID, From: e.From, To: e.To, Label: e.Label, NeedsReview: e.NeedsReview})
	}
	if !strings.EqualFold(c.DiagramID, g.ID) || !strings.EqualFold(c.BlockID, context.BlockID) || g.Kind != "mindMap" ||
		len(c.Actions) == 0 || len(c.Actions) > 32 || validateDiagramShape(g, false) != nil {
		return DiagramGraph{}, ErrInvalid
	}
	before := g
	before.Nodes, before.Edges = slices.Clone(g.Nodes), slices.Clone(g.Edges)
	key := strings.ToLower
	claimed := map[string]bool{key(c.ID): true, key(c.BlockID): true, key(c.DiagramID): true}
	for _, n := range g.Nodes {
		claimed[key(n.ID)] = true
	}
	for _, e := range g.Edges {
		claimed[key(e.ID)] = true
	}
	node := func(id string) int {
		return slices.IndexFunc(g.Nodes, func(n DiagramNode) bool { return strings.EqualFold(n.ID, id) })
	}
	incoming := func(id string) int {
		return slices.IndexFunc(g.Edges, func(e DiagramEdge) bool { return strings.EqualFold(e.To, id) })
	}
	descendants := func(id string) map[string]bool {
		found, pending := map[string]bool{}, []string{id}
		for len(pending) > 0 {
			next := key(pending[len(pending)-1])
			pending = pending[:len(pending)-1]
			if found[next] {
				continue
			}
			found[next] = true
			for _, e := range g.Edges {
				if key(e.From) == next {
					pending = append(pending, e.To)
				}
			}
		}
		return found
	}
	for _, action := range c.Actions {
		count := 0
		for _, present := range []bool{action.Update != nil, action.Move != nil, action.Merge != nil, action.Remove != nil, action.Add != nil, action.Order != nil} {
			if present {
				count++
			}
		}
		if count != 1 {
			return DiagramGraph{}, ErrInvalid
		}
		switch {
		case action.Update != nil:
			a := action.Update
			i := node(a.NodeID)
			if i < 0 || (a.Title == nil && a.Detail == nil) {
				return DiagramGraph{}, ErrInvalid
			}
			if a.Title != nil {
				g.Nodes[i].Title = *a.Title
			}
			if a.Detail != nil {
				g.Nodes[i].Detail = *a.Detail
			}
		case action.Move != nil:
			a := action.Move
			i, parent := incoming(a.NodeID), node(a.ParentID)
			if i < 0 || parent < 0 || descendants(a.NodeID)[key(a.ParentID)] {
				return DiagramGraph{}, ErrInvalid
			}
			g.Edges[i].From = g.Nodes[parent].ID
		case action.Merge != nil:
			a := action.Merge
			source, target, edge := node(a.NodeID), node(a.TargetID), incoming(a.NodeID)
			if source < 0 || target < 0 || edge < 0 || descendants(a.NodeID)[key(a.TargetID)] {
				return DiagramGraph{}, ErrInvalid
			}
			s, e := g.Nodes[source], g.Edges[edge]
			parent := node(e.From)
			if parent < 0 {
				return DiagramGraph{}, ErrInvalid
			}
			relation := "原关联：" + g.Nodes[parent].Title + " → " + s.Title
			if e.Label != "" {
				relation += "（" + e.Label + "）"
			}
			parts := []string{}
			if g.Nodes[target].Detail != "" {
				parts = append(parts, g.Nodes[target].Detail)
			}
			sourceText := s.Title
			if s.Detail != "" {
				sourceText += "\n" + s.Detail
			}
			parts = append(parts, sourceText, relation)
			g.Nodes[target].Title = strings.TrimSpace(a.Title)
			g.Nodes[target].Detail = strings.Join(parts, "\n\n")
			for _, id := range s.BlockIDs {
				if !slices.ContainsFunc(g.Nodes[target].BlockIDs, func(other string) bool { return strings.EqualFold(id, other) }) {
					g.Nodes[target].BlockIDs = append(g.Nodes[target].BlockIDs, id)
				}
			}
			g.Nodes[target].NeedsReview = g.Nodes[target].NeedsReview || s.NeedsReview || e.NeedsReview
			for i := range g.Edges {
				if strings.EqualFold(g.Edges[i].From, s.ID) {
					g.Edges[i].From = g.Nodes[target].ID
				}
			}
			g.Nodes = slices.Delete(g.Nodes, source, source+1)
			g.Edges = slices.Delete(g.Edges, edge, edge+1)
		case action.Remove != nil:
			a := action.Remove
			if incoming(a.NodeID) < 0 {
				return DiagramGraph{}, ErrInvalid
			}
			removed := descendants(a.NodeID)
			g.Nodes = slices.DeleteFunc(g.Nodes, func(n DiagramNode) bool { return removed[key(n.ID)] })
			g.Edges = slices.DeleteFunc(g.Edges, func(e DiagramEdge) bool { return removed[key(e.From)] || removed[key(e.To)] })
		case action.Add != nil:
			a := action.Add
			parent := node(a.ParentID)
			if parent < 0 || !validID(a.NodeID) || !validID(a.EdgeID) || claimed[key(a.NodeID)] || claimed[key(a.EdgeID)] || strings.EqualFold(a.NodeID, a.EdgeID) {
				return DiagramGraph{}, ErrInvalid
			}
			claimed[key(a.NodeID)], claimed[key(a.EdgeID)] = true, true
			g.Nodes = append(g.Nodes, DiagramNode{ID: a.NodeID, Title: a.Title, Detail: a.Detail})
			g.Edges = append(g.Edges, DiagramEdge{ID: a.EdgeID, From: g.Nodes[parent].ID, To: a.NodeID})
		case action.Order != nil:
			a := action.Order
			if node(a.ParentID) < 0 {
				return DiagramGraph{}, ErrInvalid
			}
			indices, edges := []int{}, map[string]DiagramEdge{}
			for i, e := range g.Edges {
				if strings.EqualFold(e.From, a.ParentID) {
					indices = append(indices, i)
					edges[key(e.ID)] = e
				}
			}
			if len(indices) != len(a.EdgeIDs) {
				return DiagramGraph{}, ErrInvalid
			}
			for i, id := range a.EdgeIDs {
				e, ok := edges[key(id)]
				if !ok {
					return DiagramGraph{}, ErrInvalid
				}
				g.Edges[indices[i]] = e
				delete(edges, key(id))
			}
		}
		if validateDiagramShape(g, false) != nil {
			return DiagramGraph{}, ErrInvalid
		}
	}
	if reflect.DeepEqual(before, g) {
		return DiagramGraph{}, ErrInvalid
	}
	return g, nil
}

// Each action contains one semantic operation. The nested shape matches the
// client's enum; absent update fields preserve the existing value.
type DiagramNodeUpdate struct {
	NodeID string  `json:"nodeID"`
	Title  *string `json:"title"`
	Detail *string `json:"detail"`
}
type DiagramNodeMove struct {
	NodeID   string `json:"nodeID"`
	ParentID string `json:"parentID"`
}
type DiagramNodeMerge struct {
	NodeID   string `json:"nodeID"`
	TargetID string `json:"targetID"`
	Title    string `json:"title"`
}
type DiagramNodeRemoval struct {
	NodeID string `json:"nodeID"`
}
type DiagramNodeAddition struct {
	NodeID   string `json:"nodeID"`
	EdgeID   string `json:"edgeID"`
	ParentID string `json:"parentID"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}
type DiagramChildOrder struct {
	ParentID string   `json:"parentID"`
	EdgeIDs  []string `json:"edgeIDs"`
}
type DiagramEditAction struct {
	Update *DiagramNodeUpdate   `json:"update,omitempty"`
	Move   *DiagramNodeMove     `json:"move,omitempty"`
	Merge  *DiagramNodeMerge    `json:"merge,omitempty"`
	Remove *DiagramNodeRemoval  `json:"remove,omitempty"`
	Add    *DiagramNodeAddition `json:"add,omitempty"`
	Order  *DiagramChildOrder   `json:"order,omitempty"`
}
type DiagramEdit struct {
	ID          string              `json:"id"`
	BlockID     string              `json:"blockID"`
	DiagramID   string              `json:"diagramID"`
	SourceID    string              `json:"sourceID"`
	Instruction string              `json:"instruction"`
	Actions     []DiagramEditAction `json:"actions"`
}

func diagramEditSchema() map[string]any {
	object := func(required []string, properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "required": required, "properties": properties, "additionalProperties": false}
	}
	text := map[string]any{"type": "string"}
	nullable := map[string]any{"type": []string{"string", "null"}}
	variant := func(name string, fields []string, properties map[string]any) map[string]any {
		return object([]string{name}, map[string]any{name: object(fields, properties)})
	}
	action := map[string]any{"anyOf": []any{
		variant("update", []string{"nodeID", "title", "detail"}, map[string]any{"nodeID": text, "title": nullable, "detail": nullable}),
		variant("move", []string{"nodeID", "parentID"}, map[string]any{"nodeID": text, "parentID": text}),
		variant("merge", []string{"nodeID", "targetID", "title"}, map[string]any{"nodeID": text, "targetID": text, "title": text}),
		variant("remove", []string{"nodeID"}, map[string]any{"nodeID": text}),
		variant("add", []string{"nodeID", "edgeID", "parentID", "title", "detail"}, map[string]any{"nodeID": text, "edgeID": text, "parentID": text, "title": text, "detail": text}),
		variant("order", []string{"parentID", "edgeIDs"}, map[string]any{"parentID": text, "edgeIDs": map[string]any{"type": "array", "maxItems": 2000, "items": text}}),
	}}
	return object([]string{"id", "blockID", "diagramID", "sourceID", "instruction", "actions"}, map[string]any{
		"id": text, "blockID": text, "diagramID": text, "sourceID": text, "instruction": text,
		"actions": map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": action},
	})
}
