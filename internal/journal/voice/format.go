package voice

import (
	"strings"
	"unicode/utf8"
)

func validateFormatContext(items []FormatContext, blocks map[string]bool) error {
	if len(items) > 8 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	totalCandidates := 0
	for _, item := range items {
		if !validID(item.ReceiptID) || seen[item.ReceiptID] || !blocks[item.BlockID] ||
			(item.State != "pending" && item.State != "applied") || len(item.Candidates) > 8 ||
			utf8.RuneCountInString(item.Title) > 40 || utf8.RuneCountInString(item.Quote) > 120 ||
			(item.State == "applied" && len(item.Candidates) != 0) {
			return ErrInvalid
		}
		seen[item.ReceiptID] = true
		ids := map[string]bool{}
		lastOrdinal := 0
		for _, candidate := range item.Candidates {
			if candidate.ID == "" || len(candidate.ID) > 80 || ids[candidate.ID] ||
				candidate.Ordinal <= lastOrdinal || candidate.Ordinal > 8 || utf8.RuneCountInString(candidate.Excerpt) > 160 {
				return ErrInvalid
			}
			lastOrdinal = candidate.Ordinal
			ids[candidate.ID] = true
		}
		totalCandidates += len(item.Candidates)
	}
	if totalCandidates > 16 {
		return ErrInvalid
	}
	return nil
}

func validateFormatResolutions(r Revision, s Snapshot, touched map[string]bool) error {
	context := map[string]FormatContext{}
	for _, item := range s.FormatContext {
		context[item.ReceiptID] = item
	}
	ids := map[string]bool{}
	for _, command := range r.FormatCommands {
		ids[command.ID] = true
	}
	receipts := map[string]bool{}
	for _, resolution := range r.FormatResolutions {
		item, ok := context[resolution.ReceiptID]
		if !ok || !validID(resolution.ID) || ids[resolution.ID] || receipts[resolution.ReceiptID] ||
			touched[item.BlockID] || resolution.Instruction == "" || utf8.RuneCountInString(resolution.Instruction) > 500 {
			return ErrInvalid
		}
		for _, correction := range r.Corrections {
			if correction.BlockID == item.BlockID {
				return ErrInvalid
			}
		}
		for _, command := range r.FormatCommands {
			if command.BlockID == item.BlockID {
				return ErrInvalid
			}
		}
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
		case "choose":
			if item.State != "pending" {
				return ErrInvalid
			}
			found := false
			for _, candidate := range item.Candidates {
				if candidate.ID == resolution.CandidateID {
					found = true
				}
			}
			if !found {
				return ErrInvalid
			}
		case "undo":
			if item.State != "applied" || resolution.CandidateID != "" {
				return ErrInvalid
			}
		case "dismiss":
			if item.State != "pending" || resolution.CandidateID != "" {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
		ids[resolution.ID] = true
		receipts[resolution.ReceiptID] = true
	}
	return nil
}

// Commands are restricted document operations, never executable instructions.
// The app resolves grapheme-safe targets and retains ambiguous edits for review.
type TextAnchor struct {
	Quote  string `json:"quote"`
	Prefix string `json:"prefix"`
	Suffix string `json:"suffix"`
}

type FormatCommand struct {
	ID          string     `json:"id"`
	BlockID     string     `json:"blockID"`
	Anchor      TextAnchor `json:"anchor"`
	SourceID    string     `json:"sourceID"`
	Instruction string     `json:"instruction"`
	Mark        string     `json:"mark"`
	Enabled     bool       `json:"enabled"`
}

type FormatCandidate struct {
	ID      string `json:"id"`
	Ordinal int    `json:"ordinal"`
	Excerpt string `json:"excerpt"`
}

type FormatContext struct {
	ReceiptID  string            `json:"receiptID"`
	State      string            `json:"state"`
	BlockID    string            `json:"blockID"`
	Title      string            `json:"title"`
	Quote      string            `json:"quote"`
	Candidates []FormatCandidate `json:"candidates"`
}

type FormatResolution struct {
	ID          string `json:"id"`
	ReceiptID   string `json:"receiptID"`
	Action      string `json:"action"`
	CandidateID string `json:"candidateID"`
	SourceID    string `json:"sourceID"`
	Instruction string `json:"instruction"`
}

func validFormatMark(mark string) bool {
	switch mark {
	case "bold", "italic", "underline", "strikethrough", "yellow", "blue", "sage":
		return true
	default:
		return false
	}
}
