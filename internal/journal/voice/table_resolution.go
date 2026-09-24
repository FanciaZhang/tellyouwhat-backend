package voice

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// Receipts describe available actions, not table contents. The app derives
// capability flags by dry-running the exact native plan against the live doc.
type TableReceiptContext struct {
	ReceiptID   string `json:"receiptID"`
	State       string `json:"state"`
	Kind        string `json:"kind"`
	BlockID     string `json:"blockID"`
	TableID     string `json:"tableID"`
	Title       string `json:"title"`
	Instruction string `json:"instruction"`
	CanConfirm  bool   `json:"canConfirm"`
	CanUndo     bool   `json:"canUndo"`
}

type TableResolution struct {
	ID          string `json:"id"`
	ReceiptID   string `json:"receiptID"`
	Action      string `json:"action"`
	SourceID    string `json:"sourceID"`
	Instruction string `json:"instruction"`
}

func validateTableReceiptContext(s Snapshot) error {
	if len(s.TableReceiptContext) > 8 {
		return ErrInvalid
	}
	used := map[string]bool{}
	known := map[string]bool{}
	for _, b := range s.Blocks {
		used[b.ID] = true
		known[b.ID] = true
	}
	for _, ids := range s.BlockComponents {
		for _, id := range ids {
			used[id] = true
		}
	}
	for _, c := range s.FormatContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.MoveContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.ParagraphContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.TableReceiptContext {
		if !validID(c.ReceiptID) || used[c.ReceiptID] || !validID(c.BlockID) || !validID(c.TableID) || c.TableID == c.BlockID || c.ReceiptID == c.TableID || c.ReceiptID == c.BlockID ||
			(c.Kind != "creation" && c.Kind != "edit") || (c.State != "proposed" && c.State != "applied" && c.State != "undone") ||
			(c.CanConfirm && c.State != "proposed" && c.State != "undone") || (c.CanUndo && c.State != "applied") ||
			utf8.RuneCountInString(c.Title) > 300 || utf8.RuneCountInString(c.Instruction) > 160 {
			return ErrInvalid
		}
		used[c.ReceiptID] = true
		// Proposed creations have no document block yet. Stale previews must
		// remain dismissible even after their original target was removed.
		if c.CanUndo || (c.CanConfirm && c.Kind == "edit") {
			if !known[c.BlockID] || !slices.Contains(s.BlockComponents[c.BlockID], c.TableID) {
				return ErrInvalid
			}
		}
	}
	return nil
}

func validateTableResolutions(r Revision, s Snapshot) error {
	if len(r.TableResolutions) == 0 {
		return nil
	}
	if len(r.TableResolutions) > 8 || len(r.TableCreations)+len(r.TableEdits)+len(r.MoveCommands)+len(r.MoveResolutions)+len(r.ParagraphCommands)+len(r.ParagraphResolutions)+len(r.FormatResolutions) > 0 {
		return ErrInvalid
	}
	used := map[string]bool{}
	seen := map[string]bool{}
	targets := map[string]bool{}
	for _, b := range s.Blocks {
		used[b.ID] = true
	}
	for _, ids := range s.BlockComponents {
		for _, id := range ids {
			used[id] = true
		}
	}
	for _, c := range s.TableReceiptContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.FormatContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.MoveContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.ParagraphContext {
		used[c.ReceiptID] = true
	}
	for _, c := range r.FormatCommands {
		used[c.ID] = true
	}
	for _, c := range r.BlockEdits {
		used[c.ID] = true
	}
	for _, resolution := range r.TableResolutions {
		i := slices.IndexFunc(s.TableReceiptContext, func(c TableReceiptContext) bool { return c.ReceiptID == resolution.ReceiptID })
		if i < 0 || !validID(resolution.ID) || used[resolution.ID] || seen[resolution.ReceiptID] || !slices.Contains(r.ConsumedSourceIDs, resolution.SourceID) || strings.TrimSpace(resolution.Instruction) == "" || utf8.RuneCountInString(resolution.Instruction) > 500 {
			return ErrInvalid
		}
		item := s.TableReceiptContext[i]
		switch resolution.Action {
		case "confirm":
			if !item.CanConfirm || (item.State != "proposed" && item.State != "undone") {
				return ErrInvalid
			}
		case "undo":
			if !item.CanUndo || item.State != "applied" {
				return ErrInvalid
			}
		case "dismiss":
			if item.State != "proposed" {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
		found := false
		for _, u := range s.PendingUtterances {
			if u.ID == resolution.SourceID && strings.Count(u.Text, resolution.Instruction) == 1 {
				found = true
			}
		}
		if !found || targets[item.TableID] {
			return ErrInvalid
		}
		linked := false
		for _, p := range r.SourcePartitions {
			if p.SourceID == resolution.SourceID {
				for _, part := range p.Segments {
					if part.Role == "instruction" && part.Text == resolution.Instruction && slices.Contains(part.BlockIDs, item.BlockID) {
						linked = true
					}
				}
			}
		}
		if !linked {
			return ErrInvalid
		}
		for _, c := range r.BlockEdits {
			if c.ID == item.BlockID {
				return ErrInvalid
			}
		}
		for _, c := range r.Corrections {
			if c.BlockID == item.BlockID {
				return ErrInvalid
			}
		}
		for _, c := range r.FormatCommands {
			if c.BlockID == item.BlockID {
				return ErrInvalid
			}
		}
		used[resolution.ID] = true
		seen[resolution.ReceiptID] = true
		targets[item.TableID] = true
	}
	return nil
}
