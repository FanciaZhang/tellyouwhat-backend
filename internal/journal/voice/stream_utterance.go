package voice

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"unicode/utf8"
)

// Speaker is provider-local to ONE segment/connection. The enclosing segment
// UUID must always accompany it; identical numbers in other segments are unrelated.
type StreamUtterance struct {
	Text                     string       `json:"text"`
	StartMilliseconds        int          `json:"startMilliseconds"`
	EndMilliseconds          int          `json:"endMilliseconds"`
	ProviderEndMilliseconds  *int         `json:"providerEndMilliseconds,omitempty"`
	ProviderStartUnavailable bool         `json:"providerStartUnavailable,omitempty"`
	Definite                 bool         `json:"definite"`
	Speaker                  string       `json:"speaker,omitempty"`
	AcousticEmotion          string       `json:"acousticEmotion,omitempty"`
	Volume                   *float64     `json:"volume,omitempty"`
	SpeechRate               *float64     `json:"speechRate,omitempty"`
	Words                    []StreamWord `json:"words,omitempty"`
}
type StreamWord struct {
	Text              string `json:"text"`
	StartMilliseconds int    `json:"startMilliseconds"`
	EndMilliseconds   int    `json:"endMilliseconds"`
}
type providerStreamUtterance struct {
	Text      string                     `json:"text"`
	Definite  bool                       `json:"definite"`
	Start     json.RawMessage            `json:"start_time"`
	End       *int                       `json:"end_time"`
	Additions map[string]json.RawMessage `json:"additions"`
	Words     []struct {
		Text  string `json:"text"`
		Start int    `json:"start_time"`
		End   int    `json:"end_time"`
	} `json:"words"`
}

func streamUtterances(source []providerStreamUtterance) []StreamUtterance {
	if len(source) > 256 {
		return nil
	}
	var result []StreamUtterance
	for index, raw := range source {
		start, unavailable, valid := streamUtteranceStart(raw.Start, index)
		if !valid || raw.End == nil || start < 0 || *raw.End < start || *raw.End > 15100 || utf8.RuneCountInString(raw.Text) > 4096 {
			continue
		}
		u := StreamUtterance{Text: raw.Text, Definite: raw.Definite, StartMilliseconds: start, EndMilliseconds: *raw.End, ProviderStartUnavailable: unavailable}
		// Metadata may be absent on provisional results. Never fabricate defaults.
		u.Speaker = streamString(raw.Additions["speaker_id"], 128)
		u.AcousticEmotion = streamString(raw.Additions["emotion"], 512)
		u.Volume = streamNumber(raw.Additions["volume"])
		u.SpeechRate = streamNumber(raw.Additions["speech_rate"])
		if len(raw.Words) <= 2048 {
			for _, w := range raw.Words {
				if w.Start >= start && w.End >= w.Start && w.End <= *raw.End && utf8.RuneCountInString(w.Text) <= 128 {
					u.Words = append(u.Words, StreamWord{w.Text, w.Start, w.End})
				}
			}
		}
		result = append(result, u)
	}
	return result
}

// The actual provider can omit the first utterance's start_time while
// returning its text, end, words and speaker. Keep that evidence, using the
// segment beginning only as a conservative replay bound. Record the omission;
// it is not an invented precise onset. Explicit null/invalid values and missing
// starts on later turns remain invalid rather than borrowing a speaker's range.
func streamUtteranceStart(raw json.RawMessage, index int) (start int, unavailable, valid bool) {
	if len(raw) == 0 {
		return 0, index == 0, index == 0
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &start) != nil {
		return 0, false, false
	}
	return start, false, true
}

func streamString(raw json.RawMessage, limit int) string {
	var value string
	if json.Unmarshal(raw, &value) != nil || utf8.RuneCountInString(value) > limit {
		return ""
	}
	return value
}
func streamNumber(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		var text string
		if json.Unmarshal(raw, &text) != nil || len(text) > 64 {
			return nil
		}
		var err error
		n, err = strconv.ParseFloat(text, 64)
		if err != nil {
			return nil
		}
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}
func boundedStreamUtterances(source []StreamUtterance, milliseconds int) []StreamUtterance {
	var result []StreamUtterance
	for _, u := range source {
		if u.StartMilliseconds < 0 || u.StartMilliseconds >= milliseconds || u.EndMilliseconds < u.StartMilliseconds || u.EndMilliseconds > milliseconds+100 {
			continue
		}
		// The live provider pads 15-second boundaries by 2 ms. Intersect with
		// captured audio, keeping the original end for provenance; reject larger errors.
		if u.EndMilliseconds > milliseconds {
			original := u.EndMilliseconds
			u.ProviderEndMilliseconds = &original
			u.EndMilliseconds = milliseconds
			words := make([]StreamWord, 0, len(u.Words))
			for _, w := range u.Words {
				if w.StartMilliseconds >= milliseconds {
					continue
				}
				w.EndMilliseconds = min(w.EndMilliseconds, milliseconds)
				words = append(words, w)
			}
			u.Words = words
		}
		result = append(result, u)
	}
	return result
}
