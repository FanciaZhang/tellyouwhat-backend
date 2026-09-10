package voice

import "unicode/utf8"

// A connection can return a rolling utterance window while result.text remains
// cumulative. Keep earlier definitive evidence, never an earlier provisional
// hypothesis. This state belongs to one ASR connection, not a person's identity.
type streamUtteranceWindow struct{ stable []StreamUtterance }

func (w *streamUtteranceWindow) merge(t Transcript) Transcript {
	merged := make([]StreamUtterance, 0, len(w.stable)+len(t.Utterances))
	for _, old := range w.stable {
		if len(t.Utterances) == 0 || old.EndMilliseconds <= t.Utterances[0].StartMilliseconds {
			merged = append(merged, old)
		}
	}
	merged = append(merged, t.Utterances...)
	characters := 0
	lastStart := -1
	for _, u := range merged {
		characters += utf8.RuneCountInString(u.Text)
		if u.StartMilliseconds < lastStart || characters > MaxContextCharacters || len(merged) > 256 {
			// Metadata is optional. Never lose the authoritative text on overflow.
			w.stable = nil
			t.Utterances = nil
			return t
		}
		lastStart = u.StartMilliseconds
	}
	w.stable = nil
	for _, u := range merged {
		if u.Definite {
			w.stable = append(w.stable, u)
		}
	}
	t.Utterances = merged
	return t
}
