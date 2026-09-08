package voice

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// ApplyRevision validates the same ordered patch contract delivered to the App.
// Media-only blocks keep their identity, text and order in the resulting document.
func ApplyRevision(s Snapshot, r Revision) ([]Block, error) {
	if err := r.Validate(s); err != nil {
		return nil, err
	}
	blocks := slices.Clone(s.Blocks)
	for _, p := range r.Patches {
		if p.AfterID == "" {
			for i := range blocks {
				if blocks[i].ID == p.ID {
					blocks[i].Text = p.Text
					break
				}
			}
		} else {
			index := slices.IndexFunc(blocks, func(b Block) bool { return b.ID == p.AfterID })
			if index < 0 {
				return nil, ErrInvalid
			}
			blocks = slices.Insert(blocks, index+1, Block{ID: p.ID, Text: p.Text})
		}
	}
	// Earlier transcription cannot discard a still-present manual replacement.
	for _, edit := range s.ManualEdits {
		if edit.After == "" || (!edit.PendingEarlierSpeech && edit.TranscriptOffset < utf8.RuneCountInString(s.Transcript)) {
			continue
		}
		old := slices.IndexFunc(s.Blocks, func(b Block) bool { return b.ID == edit.BlockID })
		next := slices.IndexFunc(blocks, func(b Block) bool { return b.ID == edit.BlockID })
		if old >= 0 && next >= 0 && strings.Contains(s.Blocks[old].Text, edit.After) && !strings.Contains(blocks[next].Text, edit.After) {
			return nil, ErrConflict
		}
	}
	return blocks, nil
}
