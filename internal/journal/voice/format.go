package voice

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

func validFormatMark(mark string) bool {
	switch mark {
	case "bold", "italic", "underline", "strikethrough", "yellow", "blue", "sage":
		return true
	default:
		return false
	}
}
