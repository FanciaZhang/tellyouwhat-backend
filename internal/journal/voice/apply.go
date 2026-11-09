package voice

import (
	"slices"
	"strings"
	"unicode"
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
	// Earlier transcription cannot replace or restore a concrete user choice.
	// A later utterance may legitimately correct it, but an already captured
	// transcript is never sufficient evidence to silently roll the edit back.
	for _, edit := range s.ManualEdits {
		if !edit.PendingEarlierSpeech && edit.TranscriptOffset < utf8.RuneCountInString(s.Transcript) {
			continue
		}
		old := slices.IndexFunc(s.Blocks, func(b Block) bool { return b.ID == edit.BlockID })
		next := slices.IndexFunc(blocks, func(b Block) bool { return b.ID == edit.BlockID })
		if old < 0 || next < 0 {
			continue
		}
		before, after := s.Blocks[old].Text, blocks[next].Text
		if hasEditorialMeaning(edit.After) {
			if strings.Count(after, edit.After) < strings.Count(before, edit.After) {
				return nil, ErrConflict
			}
		} else if hasEditorialMeaning(edit.Before) && strings.Count(after, edit.Before) > strings.Count(before, edit.Before) {
			return nil, ErrConflict
		}
	}
	return blocks, nil
}

func hasEditorialMeaning(value string) bool {
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}
