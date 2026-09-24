package voice

import (
	"slices"
	"strings"
	"unicode/utf8"
)

type ParagraphContext struct {
	ReceiptID   string   `json:"receiptID"`
	State       string   `json:"state"`
	Kind        string   `json:"kind"`
	BlockIDs    []string `json:"blockIDs"`
	Instruction string   `json:"instruction"`
	CanConfirm  bool     `json:"canConfirm"`
}
type ParagraphResolution struct {
	ID          string `json:"id"`
	ReceiptID   string `json:"receiptID"`
	Action      string `json:"action"`
	SourceID    string `json:"sourceID"`
	Instruction string `json:"instruction"`
}

func validateParagraphContext(items []ParagraphContext, known map[string]bool) error {
	if len(items) > 8 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, c := range items {
		if !validID(c.ReceiptID) || seen[c.ReceiptID] || (c.Kind != "split" && c.Kind != "merge") ||
			(c.State != "proposed" && c.State != "applied") || (c.CanConfirm && c.State != "proposed") ||
			len(c.BlockIDs) == 0 || len(c.BlockIDs) > 64 || utf8.RuneCountInString(c.Instruction) > 160 {
			return ErrInvalid
		}
		seen[c.ReceiptID] = true
		ids := map[string]bool{}
		for _, id := range c.BlockIDs {
			if !validID(id) || ids[id] || ((c.CanConfirm || c.State == "applied") && !known[id]) {
				return ErrInvalid
			}
			ids[id] = true
		}
	}
	return nil
}

func validateParagraphResolutions(r Revision, s Snapshot, touched map[string]bool) error {
	if len(r.ParagraphResolutions) > 8 {
		return ErrInvalid
	}
	if len(r.ParagraphResolutions) == 0 {
		return nil
	}
	if len(r.ParagraphCommands) > 0 || len(r.MoveCommands) > 0 || len(r.MoveResolutions) > 0 || len(r.FormatResolutions) > 0 {
		return ErrInvalid
	}
	used, receipts, targets, consumed := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range r.FormatCommands {
		used[c.ID] = true
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
	for _, id := range r.ConsumedSourceIDs {
		consumed[id] = true
	}
	for _, c := range r.ParagraphResolutions {
		i := slices.IndexFunc(s.ParagraphContext, func(item ParagraphContext) bool { return item.ReceiptID == c.ReceiptID })
		if i < 0 || !validID(c.ID) || used[c.ID] || receipts[c.ReceiptID] || !consumed[c.SourceID] || strings.TrimSpace(c.Instruction) == "" || utf8.RuneCountInString(c.Instruction) > 500 {
			return ErrInvalid
		}
		item := s.ParagraphContext[i]
		evidence := false
		for _, u := range s.PendingUtterances {
			if u.ID == c.SourceID && strings.Count(u.Text, c.Instruction) == 1 {
				evidence = true
			}
		}
		if !evidence {
			return ErrInvalid
		}
		switch c.Action {
		case "confirm":
			if item.State != "proposed" || !item.CanConfirm {
				return ErrInvalid
			}
		case "dismiss":
			if item.State != "proposed" {
				return ErrInvalid
			}
		case "undo":
			if item.State != "applied" {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
		for _, id := range item.BlockIDs {
			if targets[id] || touched[id] {
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
			targets[id] = true
		}
		used[c.ID] = true
		receipts[c.ReceiptID] = true
	}
	return nil
}
