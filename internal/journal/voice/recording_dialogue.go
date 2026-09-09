package voice

import "strings"

// Dialogue is a view over the source turns, not a second model interpretation.
// Every turn retains its utterance IDs and times. Unknown labels stay unknown.
type RecordingDialogueTurn struct {
	SpeakerID         string   `json:"speakerID"`
	SpeakerName       string   `json:"speakerName"`
	UtteranceIDs      []string `json:"utteranceIDs"`
	StartMilliseconds int      `json:"startMilliseconds"`
	EndMilliseconds   int      `json:"endMilliseconds"`
	Text              string   `json:"text"`
}

func RenderRecordingDialogue(r RecordingContext) ([]RecordingDialogueTurn, error) {
	if err := r.Validate(r.Analysis.Text); err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, s := range r.Speakers {
		names[s.ID] = s.Name
	}
	turns := []RecordingDialogueTurn{}
	for _, u := range r.Analysis.Utterances {
		name := names[u.Speaker]
		if name == "" {
			name = "未确认声音"
		}
		// Empty speaker means unknown, not evidence that two turns share a speaker.
		if len(turns) > 0 && u.Speaker != "" && turns[len(turns)-1].SpeakerID == u.Speaker {
			last := &turns[len(turns)-1]
			last.Text += u.Text
			last.EndMilliseconds = u.EndMilliseconds
			last.UtteranceIDs = append(last.UtteranceIDs, u.ID)
		} else {
			turns = append(turns, RecordingDialogueTurn{u.Speaker, name, []string{u.ID}, u.StartMilliseconds, u.EndMilliseconds, u.Text})
		}
	}
	return turns, nil
}

// The model may place the dialogue among existing paragraphs, but may not
// paraphrase its speakers/turns or silently drop a contribution.
func RecordingDialogueText(r RecordingContext) (string, error) {
	turns, err := RenderRecordingDialogue(r)
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(turns))
	for _, turn := range turns {
		lines = append(lines, turn.SpeakerName+"："+turn.Text)
	}
	return strings.Join(lines, "\n\n"), nil
}
func ValidateRecordingDialogueRevision(s Snapshot, r Revision) error {
	if s.RecordingContext == nil || s.RecordingContext.Mode != "dialogue" {
		return nil
	}
	if err := r.Validate(s); err != nil {
		return err
	}
	if len(r.Patches) == 0 && len(r.Questions) > 0 {
		return nil
	}
	canonical, err := RecordingDialogueText(*s.RecordingContext)
	if err != nil || canonical == "" {
		return ErrInvalid
	}
	blocks := append([]Block(nil), s.Blocks...)
	for _, patch := range r.Patches {
		for i, block := range blocks {
			if patch.AfterID == "" && block.ID == patch.ID {
				blocks[i].Text = patch.Text
				break
			}
			if patch.AfterID != "" && block.ID == patch.AfterID {
				blocks = append(blocks, Block{})
				copy(blocks[i+2:], blocks[i+1:])
				blocks[i+1] = Block{patch.ID, patch.Text}
				break
			}
		}
	}
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		texts = append(texts, block.Text)
	}
	if strings.Count(strings.Join(texts, "\n\n"), canonical) != 1 {
		return ErrInvalid
	}
	return nil
}
