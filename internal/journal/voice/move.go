package voice

import (
	"slices"
	"strings"
	"unicode/utf8"
)

type MoveCommand struct {
	ID          string   `json:"id"`
	BlockIDs    []string `json:"blockIDs"`
	AfterID     *string  `json:"afterID"`
	SourceID    string   `json:"sourceID"`
	Instruction string   `json:"instruction"`
}

func validateParallelGroups(s Snapshot) error {
	positions := map[string]int{}
	for i, b := range s.Blocks {
		positions[b.ID] = i
	}
	seen := map[string]bool{}
	for _, group := range s.ParallelGroups {
		if len(group) < 2 {
			return ErrInvalid
		}
		previous := -1
		for _, id := range group {
			position, exists := positions[id]
			if !exists || seen[id] || (previous >= 0 && position != previous+1) {
				return ErrInvalid
			}
			seen[id] = true
			previous = position
		}
	}
	return nil
}

func moveTargets(c MoveCommand, s Snapshot) []string {
	targets := map[string]bool{}
	for _, id := range c.BlockIDs {
		targets[id] = true
	}
	for _, group := range s.ParallelGroups {
		if slices.ContainsFunc(group, func(id string) bool { return targets[id] }) {
			for _, id := range group {
				targets[id] = true
			}
		}
	}
	result := []string{}
	for _, b := range s.Blocks {
		if targets[b.ID] {
			result = append(result, b.ID)
		}
	}
	return result
}

func validateMoves(r Revision, s Snapshot, touched map[string]bool) error {
	if len(r.MoveCommands) > 8 {
		return ErrInvalid
	}
	known, operations, consumed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	sources := map[string]string{}
	for _, b := range s.Blocks {
		known[b.ID] = true
	}
	for _, u := range s.PendingUtterances {
		sources[u.ID] = u.Text
	}
	for _, id := range r.ConsumedSourceIDs {
		consumed[id] = true
	}
	for _, c := range r.FormatCommands {
		operations[c.ID] = true
	}
	for _, c := range r.FormatResolutions {
		operations[c.ID] = true
	}
	for _, c := range r.MoveCommands {
		if !validID(c.ID) || operations[c.ID] || len(c.BlockIDs) == 0 || len(c.BlockIDs) > 32 ||
			!consumed[c.SourceID] || strings.TrimSpace(c.Instruction) == "" ||
			utf8.RuneCountInString(c.Instruction) > 500 || strings.Count(sources[c.SourceID], c.Instruction) != 1 {
			return ErrInvalid
		}
		operations[c.ID] = true
		requested := map[string]bool{}
		for _, id := range c.BlockIDs {
			if !known[id] || requested[id] {
				return ErrInvalid
			}
			requested[id] = true
		}
		targets := moveTargets(c, s)
		destination := ""
		if c.AfterID != nil {
			destination = *c.AfterID
			if !known[destination] {
				return ErrInvalid
			}
			for _, group := range s.ParallelGroups {
				if slices.Contains(group, destination) {
					destination = group[len(group)-1]
				}
			}
			if slices.Contains(targets, destination) {
				return ErrInvalid
			}
		}
		affected := append(append([]string{}, targets...), destination)
		for _, group := range s.ParallelGroups {
			if slices.Contains(group, destination) {
				affected = append(affected, group...)
			}
		}
		for _, id := range affected {
			if touched[id] {
				return ErrInvalid
			}
			for _, edit := range r.Corrections {
				if edit.BlockID == id {
					return ErrInvalid
				}
			}
			for _, edit := range r.FormatCommands {
				if edit.BlockID == id {
					return ErrInvalid
				}
			}
		}
		for _, resolution := range r.FormatResolutions {
			for _, item := range s.FormatContext {
				if item.ReceiptID == resolution.ReceiptID {
					for _, id := range append([]string{item.BlockID}, item.AdditionalBlockIDs...) {
						if slices.Contains(affected, id) {
							return ErrInvalid
						}
					}
				}
			}
		}
		before, after := []string{}, []string{}
		for _, b := range s.Blocks {
			before = append(before, b.ID)
			if !slices.Contains(targets, b.ID) {
				after = append(after, b.ID)
			}
		}
		insertion := slices.Index(after, destination) + 1
		after = slices.Insert(after, insertion, targets...)
		if slices.Equal(before, after) {
			return ErrInvalid
		}
	}
	return nil
}
