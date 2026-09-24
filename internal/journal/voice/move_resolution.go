package voice

import (
	"slices"
	"strings"
	"unicode/utf8"
)

type MoveContext struct {
	ReceiptID   string   `json:"receiptID"`
	State       string   `json:"state"`
	BlockIDs    []string `json:"blockIDs"`
	Instruction string   `json:"instruction"`
	CanConfirm  bool     `json:"canConfirm"`
}
type MoveResolution struct {
	ID          string `json:"id"`
	ReceiptID   string `json:"receiptID"`
	Action      string `json:"action"`
	SourceID    string `json:"sourceID"`
	Instruction string `json:"instruction"`
}

func validateMoveContext(items []MoveContext, known map[string]bool) error {
	if len(items) > 8 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, item := range items {
		if !validID(item.ReceiptID) || seen[item.ReceiptID] || len(item.BlockIDs) == 0 || len(item.BlockIDs) > len(known) ||
			(item.State != "proposed" && item.State != "applied") || (item.CanConfirm && item.State != "proposed") ||
			utf8.RuneCountInString(item.Instruction) > 160 {
			return ErrInvalid
		}
		seen[item.ReceiptID] = true
		blocks := map[string]bool{}
		for _, id := range item.BlockIDs {
			if !known[id] || blocks[id] {
				return ErrInvalid
			}
			blocks[id] = true
		}
	}
	return nil
}

func validateMoveResolutions(r Revision, s Snapshot, touched map[string]bool) error {
	if len(r.MoveResolutions) > 8 || (len(r.MoveResolutions) > 0 && len(r.MoveCommands) > 0) {
		return ErrInvalid
	}
	operations, receipts, consumed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range r.FormatCommands {
		operations[c.ID] = true
	}
	for _, c := range r.FormatResolutions {
		operations[c.ID] = true
	}
	for _, item := range s.MoveContext {
		operations[item.ReceiptID] = true
	}
	for _, item := range s.FormatContext {
		operations[item.ReceiptID] = true
	}
	for _, id := range r.ConsumedSourceIDs {
		consumed[id] = true
	}
	changing := 0
	for _, resolution := range r.MoveResolutions {
		index := slices.IndexFunc(s.MoveContext, func(c MoveContext) bool { return c.ReceiptID == resolution.ReceiptID })
		if index < 0 || !validID(resolution.ID) || operations[resolution.ID] || receipts[resolution.ReceiptID] ||
			!consumed[resolution.SourceID] || strings.TrimSpace(resolution.Instruction) == "" || utf8.RuneCountInString(resolution.Instruction) > 500 {
			return ErrInvalid
		}
		item := s.MoveContext[index]
		evidence := false
		for _, source := range s.PendingUtterances {
			if source.ID == resolution.SourceID && strings.Count(source.Text, resolution.Instruction) == 1 {
				evidence = true
			}
		}
		if !evidence {
			return ErrInvalid
		}
		switch resolution.Action {
		case "confirm":
			if item.State != "proposed" || !item.CanConfirm {
				return ErrInvalid
			}
			changing++
		case "dismiss":
			if item.State != "proposed" {
				return ErrInvalid
			}
		case "undo":
			if item.State != "applied" {
				return ErrInvalid
			}
			changing++
		default:
			return ErrInvalid
		}
		if changing > 1 {
			return ErrInvalid
		}
		for _, id := range item.BlockIDs {
			if touched[id] {
				return ErrInvalid
			}
			for _, c := range r.Corrections {
				if c.BlockID == id {
					return ErrInvalid
				}
			}
			for _, c := range r.FormatCommands {
				if c.BlockID == id {
					return ErrInvalid
				}
			}
			for _, c := range r.FormatResolutions {
				for _, f := range s.FormatContext {
					if f.ReceiptID == c.ReceiptID && (f.BlockID == id || slices.Contains(f.AdditionalBlockIDs, id)) {
						return ErrInvalid
					}
				}
			}
		}
		operations[resolution.ID] = true
		receipts[resolution.ReceiptID] = true
	}
	return nil
}
