package readaloud

import (
	"context"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/google/uuid"
)

// Positions refer to the exact synthesis text, never normalized spoken words.
type Timing struct {
	StartMilliseconds int `json:"startMilliseconds"`
	EndMilliseconds   int `json:"endMilliseconds"`
	UTF16Location     int `json:"utf16Location"`
	UTF16Length       int `json:"utf16Length"`
}

type subtitle struct {
	Text  string `json:"text"`
	Words []struct {
		Word  string  `json:"word"`
		Start float64 `json:"startTime"`
		End   float64 `json:"endTime"`
	} `json:"words"`
}

func utf16Length(text string) int { return len(utf16.Encode([]rune(text))) }

func alignedTimings(text string, subtitles []subtitle) []Timing {
	ordered := slices.Clone(subtitles)
	slices.SortStableFunc(ordered, func(a, b subtitle) int {
		if len(a.Words) == 0 {
			if len(b.Words) == 0 {
				return 0
			}
			return 1
		}
		if len(b.Words) == 0 {
			return -1
		}
		if a.Words[0].Start < b.Words[0].Start {
			return -1
		}
		if a.Words[0].Start > b.Words[0].Start {
			return 1
		}
		return 0
	})
	var result []Timing
	cursor, lastEnd := 0, 0
	for _, sentence := range ordered {
		body := strings.TrimSpace(sentence.Text)
		if body == "" || len(sentence.Words) == 0 || len(sentence.Words) > MaximumTextBytes || int(math.Round(sentence.Words[0].Start*1000)) < lastEnd {
			continue
		}
		index := strings.Index(text[cursor:], body)
		if index < 0 {
			continue
		}
		start, wordCursor := cursor+index, 0
		for _, word := range sentence.Words {
			if word.Word == "" || word.Start < 0 {
				continue
			}
			if math.IsNaN(word.Start) || math.IsNaN(word.End) || math.IsInf(word.Start, 0) || math.IsInf(word.End, 0) || word.End <= word.Start || word.End > 240 {
				continue
			}
			location := strings.Index(body[wordCursor:], word.Word)
			if location < 0 {
				continue
			}
			location += wordCursor
			cue := Timing{int(math.Round(word.Start * 1000)), int(math.Round(word.End * 1000)), utf16Length(text[:start+location]), utf16Length(word.Word)}
			if cue.StartMilliseconds < lastEnd || cue.EndMilliseconds <= cue.StartMilliseconds {
				continue
			}
			result = append(result, cue)
			lastEnd = cue.EndMilliseconds
			wordCursor = location + len(word.Word)
		}
		cursor = start + len(body)
	}
	return result
}

// Unsupported subtitle languages use real phrase audio boundaries, not a
// character/time estimate. All parts share the original quota reservation.
func (provider *Provider) synthesizePhrases(ctx context.Context, input Synthesis, emit func([]byte) error) (total SynthesisUsage, err error) {
	total.UsageKnown = true
	var timings []Timing
	for start := 0; start < len(input.Text); {
		end, boundary := start, start
		for offset, r := range input.Text[start:] {
			size := len(string(r))
			if offset+size > 120 {
				break
			}
			end = start + offset + size
			if unicode.IsSpace(r) || strings.ContainsRune("。！？.!?；;，,", r) {
				boundary = end
			}
		}
		if end < len(input.Text) && boundary > start {
			end = boundary
		}
		part := input
		part.RequestID = uuid.NewString()
		part.Text = input.Text[start:end]
		part.OnTimings = nil
		before := total.AudioBytes
		if strings.TrimSpace(part.Text) == "" {
			start = end
			continue
		}
		usage, callErr := provider.synthesize(ctx, part, func(data []byte) error {
			if len(data) > MaximumAudioBytes-total.AudioBytes {
				return ErrIncomplete
			}
			total.AudioBytes += len(data)
			return emit(data)
		})
		total.Characters += usage.Characters
		total.UsageKnown = total.UsageKnown && usage.UsageKnown
		total.LogID = usage.LogID
		if callErr != nil {
			return total, callErr
		}
		timings = append(timings, Timing{before * 1000 / (SampleRate * 2), total.AudioBytes * 1000 / (SampleRate * 2), utf16Length(input.Text[:start]), utf16Length(part.Text)})
		if err = input.OnTimings(slices.Clone(timings)); err != nil {
			return total, err
		}
		start = end
	}
	return total, nil
}

func validTimings(timings []Timing, length int) bool {
	if len(timings) == 0 || len(timings) > MaximumTextBytes {
		return false
	}
	end, location := 0, 0
	for _, cue := range timings {
		if cue.StartMilliseconds < end || cue.EndMilliseconds <= cue.StartMilliseconds || cue.EndMilliseconds > 240000 || cue.UTF16Location < location || cue.UTF16Length <= 0 || cue.UTF16Location > length-cue.UTF16Length {
			return false
		}
		end = cue.EndMilliseconds
		location = cue.UTF16Location + cue.UTF16Length
	}
	return true
}

func supportsSubtitles(input Synthesis) bool {
	if input.Dialect != "" || (input.Language != "" && input.Language != "zh-cn" && input.Language != "en") {
		return false
	}
	for _, r := range input.Text {
		if unicode.IsLetter(r) && r > unicode.MaxASCII && !unicode.Is(unicode.Han, r) {
			return false
		}
	}
	return true
}
