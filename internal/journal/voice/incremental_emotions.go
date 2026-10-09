package voice

import "strings"

// Decorative emotion suggestions are optional. Discard unsupported model guesses
// instead of losing an otherwise valid manuscript; never invent acoustic evidence.
func groundedIncrementalEmotions(r Revision, s Snapshot) Revision {
	evidence := map[string]bool{}
	for _, source := range s.PendingUtterances {
		if strings.TrimSpace(source.AcousticEmotion) != "" {
			evidence[source.ID] = true
		}
	}
	kept := make([]EmotionPlacement, 0, len(r.Emotions))
	for _, emotion := range r.Emotions {
		if evidence[emotion.SourceID] {
			kept = append(kept, emotion)
		}
	}
	r.Emotions = kept
	if len(evidence) == 0 {
		r.OverallEmotion = ""
	}
	return r
}
