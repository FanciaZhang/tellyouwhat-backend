package voice

import (
	"errors"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var errRecognitionContext = errors.New("speech_context_too_large")
var errEmptyCompletedUtterance = errors.New("speech_empty_utterance_after_text")

// A connection can return a rolling utterance window while result.text remains
// cumulative. Keep earlier definitive evidence, never an earlier provisional
// hypothesis. This state belongs to one ASR connection, not a person's identity.
type streamUtteranceWindow struct {
	stable  []StreamUtterance
	pending []StreamUtterance
}

func (w *streamUtteranceWindow) merge(t Transcript) (Transcript, error) {
	incoming := make([]StreamUtterance, 0, len(t.Utterances))
	previousStart := -1
	for _, u := range t.Utterances {
		if u.StartMilliseconds < previousStart {
			return Transcript{}, errRecognitionContext
		}
		previousStart = u.StartMilliseconds
		if u.Definite && strings.TrimSpace(u.Text) == "" {
			for _, previous := range w.pending {
				if u.StartMilliseconds == previous.StartMilliseconds && strings.TrimSpace(previous.Text) != "" {
					// A real second-pass response can return an empty completed
					// placeholder for an audible provisional sentence. It is not
					// permission to erase that sentence, nor proof that the old
					// hypothesis is final. Preserve the App's last source and let
					// bounded transport recovery recognize the original audio.
					return Transcript{}, errEmptyCompletedUtterance
				}
			}
		}
		if strings.TrimSpace(u.Text) != "" {
			incoming = append(incoming, u)
		}
	}
	merged := make([]StreamUtterance, 0, len(w.stable)+len(t.Utterances))
	position, retained := 0, 0
	for _, old := range w.stable {
		for position < len(incoming) && incoming[position].EndMilliseconds <= old.StartMilliseconds && incoming[position].StartMilliseconds != old.StartMilliseconds {
			position++
		}
		replaced := position < len(incoming) && (incoming[position].StartMilliseconds == old.StartMilliseconds ||
			(incoming[position].StartMilliseconds < old.EndMilliseconds && incoming[position].EndMilliseconds > old.StartMilliseconds))
		if !replaced {
			merged = append(merged, old)
			retained++
		}
	}
	merged = append(merged, incoming...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].StartMilliseconds < merged[j].StartMilliseconds })
	characters := 0
	lastStart := -1
	for _, u := range merged {
		characters += utf8.RuneCountInString(u.Text)
		if u.StartMilliseconds < lastStart || characters > MaxContextCharacters || len(merged) > maxRecognitionUtterances {
			// Fail explicitly while retaining the preceding canonical evidence.
			// Flattening to a text blob would silently erase speaker attribution.
			return Transcript{}, errRecognitionContext
		}
		lastStart = u.StartMilliseconds
	}
	// A provider's full later window may omit a previously definite turn or
	// return an empty placeholder for it. Restore only confirmed evidence, and
	// only when the incoming full text is exactly explained by the new turns.
	// Never promote a provisional hypothesis or rescue an empty final response.
	if retained > 0 && t.Text != "" {
		var newText, canonical strings.Builder
		for _, u := range incoming {
			newText.WriteString(u.Text)
		}
		for _, u := range merged {
			canonical.WriteString(u.Text)
		}
		compact := func(value string) string {
			return strings.Map(func(c rune) rune {
				if unicode.IsSpace(c) {
					return -1
				}
				return c
			}, value)
		}
		if compact(t.Text) == compact(newText.String()) && compact(t.Text) != compact(canonical.String()) {
			if t.ProviderText == "" {
				t.ProviderText = t.Text
			}
			t.Text = canonical.String()
			t.Stable = ""
			for _, u := range merged {
				if u.Definite {
					t.Stable += u.Text
				}
			}
		}
	}
	w.stable = nil
	w.pending = nil
	for _, u := range merged {
		if u.Definite {
			w.stable = append(w.stable, u)
		} else {
			w.pending = append(w.pending, u)
		}
	}
	t.Utterances = merged
	return t, nil
}
