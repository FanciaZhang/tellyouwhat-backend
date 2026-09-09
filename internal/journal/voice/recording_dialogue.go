package voice

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
