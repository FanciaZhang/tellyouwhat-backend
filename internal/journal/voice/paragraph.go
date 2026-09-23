package voice

import (
	"slices"
	"strings"
	"unicode/utf8"
)

type ParagraphCommand struct {
	ID                 string     `json:"id"`
	Kind               string     `json:"kind"`
	BlockIDs           []string   `json:"blockIDs"`
	Anchor             TextAnchor `json:"anchor"`
	Edge               string     `json:"edge"`
	Separator          string     `json:"separator"`
	ComponentsToSecond []string   `json:"componentsToSecond"`
	SourceID           string     `json:"sourceID"`
	Instruction        string     `json:"instruction"`
}

// Resolve against the complete snapshot, not the model's bounded excerpts.
// The native app repeats resolution with extended-grapheme boundaries.
func paragraphBoundary(text string, anchor TextAnchor, edge string) (int, bool) {
	if anchor.Quote == "" || utf8.RuneCountInString(anchor.Quote)+utf8.RuneCountInString(anchor.Prefix)+utf8.RuneCountInString(anchor.Suffix) > 6000 {
		return 0, false
	}
	position, matches := 0, 0
	for cursor := 0; cursor < len(text); {
		relative := strings.Index(text[cursor:], anchor.Quote)
		if relative < 0 {
			break
		}
		start := cursor + relative
		end := start + len(anchor.Quote)
		if strings.HasSuffix(text[:start], anchor.Prefix) && strings.HasPrefix(text[end:], anchor.Suffix) {
			matches++
			position = start
			if edge == "after" {
				position = end
			}
		}
		_, width := utf8.DecodeRuneInString(text[start:])
		cursor = start + width
	}
	return position, matches == 1 && position > 0 && position < len(text) && (edge == "before" || edge == "after")
}

func validateParagraphs(r Revision, s Snapshot, touched map[string]bool) error {
	if len(r.ParagraphCommands) > 8 {
		return ErrInvalid
	}
	positions, blocks := map[string]int{}, map[string]Block{}
	for index, block := range s.Blocks {
		positions[block.ID] = index
		blocks[block.ID] = block
	}
	consumed, sources, used, conflicting := map[string]bool{}, map[string]string{}, map[string]bool{}, map[string]bool{}
	for _, id := range r.ConsumedSourceIDs {
		consumed[id] = true
	}
	for _, turn := range s.PendingUtterances {
		sources[turn.ID] = turn.Text
	}
	for id, value := range touched {
		conflicting[id] = value
	}
	for _, edit := range r.Corrections {
		conflicting[edit.BlockID] = true
	}
	for _, c := range r.FormatCommands {
		used[c.ID] = true
		conflicting[c.BlockID] = true
	}
	for _, c := range r.FormatResolutions {
		used[c.ID] = true
		for _, item := range s.FormatContext {
			if item.ReceiptID == c.ReceiptID {
				conflicting[item.BlockID] = true
				for _, id := range item.AdditionalBlockIDs {
					conflicting[id] = true
				}
			}
		}
	}
	for _, c := range r.MoveCommands {
		used[c.ID] = true
		for _, id := range moveTargets(c, s) {
			conflicting[id] = true
		}
		if c.AfterID != nil {
			conflicting[*c.AfterID] = true
			for _, group := range s.ParallelGroups {
				if slices.Contains(group, *c.AfterID) {
					for _, id := range group {
						conflicting[id] = true
					}
				}
			}
		}
	}
	for _, c := range r.MoveResolutions {
		used[c.ID] = true
		for _, item := range s.MoveContext {
			if item.ReceiptID == c.ReceiptID {
				for _, id := range item.BlockIDs {
					conflicting[id] = true
				}
			}
		}
	}
	for _, item := range s.MoveContext {
		used[item.ReceiptID] = true
	}
	for _, item := range s.FormatContext {
		used[item.ReceiptID] = true
	}
	for _, c := range r.ParagraphCommands {
		if !validID(c.ID) || used[c.ID] || !consumed[c.SourceID] || strings.TrimSpace(c.Instruction) == "" ||
			utf8.RuneCountInString(c.Instruction) > 500 || strings.Count(sources[c.SourceID], c.Instruction) != 1 ||
			len(c.BlockIDs) == 0 || len(c.BlockIDs) > 32 {
			return ErrInvalid
		}
		used[c.ID] = true
		seen := map[string]bool{}
		for _, id := range c.BlockIDs {
			if _, exists := blocks[id]; !exists || seen[id] || conflicting[id] || slices.Contains(s.MediaOnlyBlockIDs, id) {
				return ErrInvalid
			}
			seen[id] = true
		}
		switch c.Kind {
		case "split":
			if len(c.BlockIDs) != 1 || c.Separator != "" {
				return ErrInvalid
			}
			block := blocks[c.BlockIDs[0]]
			if _, ok := paragraphBoundary(block.Text, c.Anchor, c.Edge); !ok {
				return ErrInvalid
			}
			components := map[string]bool{}
			for _, id := range c.ComponentsToSecond {
				if components[id] || !slices.Contains(s.BlockComponents[block.ID], id) {
					return ErrInvalid
				}
				components[id] = true
			}
		case "merge":
			if len(c.BlockIDs) < 2 || c.Anchor != (TextAnchor{}) || c.Edge != "" ||
				(c.Separator != "" && c.Separator != " ") || len(c.ComponentsToSecond) != 0 {
				return ErrInvalid
			}
			first := blocks[c.BlockIDs[0]]
			for offset, id := range c.BlockIDs {
				if positions[id] != positions[first.ID]+offset || s.ParallelColumns[id] != s.ParallelColumns[first.ID] {
					return ErrInvalid
				}
				for _, group := range s.ParallelGroups {
					if slices.Contains(group, id) && s.ParallelColumns[first.ID] == "" {
						return ErrInvalid
					}
				}
			}
		default:
			return ErrInvalid
		}
		for id := range seen {
			conflicting[id] = true
		}
	}
	return nil
}

func validateParagraphColumns(s Snapshot) error {
	covered, used := map[string]bool{}, map[string]bool{}
	for _, group := range s.ParallelGroups {
		present := false
		for _, id := range group {
			present = present || s.ParallelColumns[id] != ""
		}
		if !present {
			continue
		}
		columns := []string{}
		for _, id := range group {
			column := s.ParallelColumns[id]
			if column == "" {
				return ErrInvalid
			}
			covered[id] = true
			if len(columns) == 0 || columns[len(columns)-1] != column {
				columns = append(columns, column)
			}
		}
		if len(columns) != 2 || used[columns[0]] || used[columns[1]] {
			return ErrInvalid
		}
		used[columns[0]], used[columns[1]] = true, true
	}
	for id := range s.ParallelColumns {
		if !covered[id] {
			return ErrInvalid
		}
	}
	return nil
}
