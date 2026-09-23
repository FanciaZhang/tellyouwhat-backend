// Package voice owns the journal-only speech protocol. Provider credentials and
// billing decisions never come from the client.
package voice

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const Version = "journal-voice-v2"
const MonthlyMilliseconds = 120 * 60 * 1000
const SessionMilliseconds = 30 * 60 * 1000
const MaxSegmentBytes = 15 * 32000 // PCM16, mono, 16 kHz
const MaxContextCharacters = 60000
const MaxPendingUtterances = 512
const MaxRewriteSourceCharacters = 6000
const MaxRewriteContextCharacters = 4000

var ErrQuota = errors.New("voice_quota_exhausted")
var ErrBusy = errors.New("voice_session_busy")
var ErrConflict = errors.New("voice_revision_conflict")
var ErrInvalid = errors.New("voice_invalid_request")

type Block struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}
type Word struct {
	Text              string `json:"text"`
	StartMilliseconds int    `json:"startMilliseconds"`
	EndMilliseconds   int    `json:"endMilliseconds"`
}
type Utterance struct {
	ID                       string  `json:"id,omitempty"`
	Text                     string  `json:"text"`
	StartMilliseconds        int     `json:"startMilliseconds"`
	EndMilliseconds          int     `json:"endMilliseconds"`
	ProviderEndMilliseconds  *int    `json:"providerEndMilliseconds,omitempty"`
	ProviderStartUnavailable *bool   `json:"providerStartUnavailable,omitempty"`
	Definite                 bool    `json:"definite"`
	Speaker                  string  `json:"speaker,omitempty"`
	AcousticEmotion          string  `json:"acousticEmotion,omitempty"`
	Volume                   float64 `json:"volume,omitempty"`
	SpeechRate               float64 `json:"speechRate,omitempty"`
	Words                    []Word  `json:"words,omitempty"`
}

// SourceUtterance is the app-owned, recording-wide identity used by the
// incremental editor. Provider speaker labels remain evidence only; Person is
// present solely after the user explicitly names or assigns the voice.
type SourceUtterance struct {
	ID                string  `json:"id"`
	Text              string  `json:"text"`
	Speaker           string  `json:"speaker,omitempty"`
	Person            string  `json:"person,omitempty"`
	StartMilliseconds int     `json:"startMilliseconds"`
	EndMilliseconds   int     `json:"endMilliseconds"`
	AcousticEmotion   string  `json:"acousticEmotion,omitempty"`
	Volume            float64 `json:"volume,omitempty"`
	SpeechRate        float64 `json:"speechRate,omitempty"`
}
type Snapshot struct {
	Revision          int               `json:"revision"`
	Blocks            []Block           `json:"blocks"`
	Transcript        string            `json:"transcript"`
	EditedBlockIDs    []string          `json:"editedBlockIDs"`
	MediaOnlyBlockIDs []string          `json:"mediaOnlyBlockIDs"`
	PendingUtterances []SourceUtterance `json:"pendingUtterances"`
	Words             []string          `json:"words"`
	WritingStyle      string            `json:"writingStyle"`
}
type Patch struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Empty afterID means replace an existing block; insertions need a new UUID.
	AfterID string `json:"afterID"`
}
type Revision struct {
	BaseRevision       int       `json:"baseRevision"`
	TranscriptRevision int       `json:"transcriptRevision"`
	Patches            []Patch   `json:"patches"`
	Passages           []Passage `json:"passages"`
	ConsumedSourceIDs  []string  `json:"consumedSourceIDs"`
	Questions          []string  `json:"questions"`
	Emotions           []Emotion `json:"emotions"`
	OverallEmotion     string    `json:"overallEmotion"`
}
type Passage struct {
	BlockID        string   `json:"blockID"`
	ParagraphIndex int      `json:"paragraphIndex"`
	SourceIDs      []string `json:"sourceIDs"`
}
type Emotion struct {
	BlockID    string `json:"blockID"`
	AnchorText string `json:"anchorText"`
	SourceID   string `json:"sourceID"`
	Kind       string `json:"kind"`
}
type Receipt struct {
	SegmentID    string      `json:"segmentID"`
	SHA256       string      `json:"sha256"`
	Text         string      `json:"text"`
	Milliseconds int         `json:"milliseconds"`
	Utterances   []Utterance `json:"utterances,omitempty"`
}
type Event struct {
	Type                  string      `json:"type"`
	SegmentID             string      `json:"segmentID,omitempty"`
	Text                  string      `json:"text,omitempty"`
	Stable                string      `json:"stable,omitempty"`
	Utterances            []Utterance `json:"utterances,omitempty"`
	Receipt               *Receipt    `json:"receipt,omitempty"`
	Revision              *Revision   `json:"revision,omitempty"`
	RemainingMilliseconds int         `json:"remainingMilliseconds"`
	Code                  string      `json:"code,omitempty"`
}
type Frame struct {
	Type      string    `json:"type"`
	SegmentID string    `json:"segmentID,omitempty"`
	PCM       []byte    `json:"pcm,omitempty"`
	Final     bool      `json:"final,omitempty"`
	Snapshot  *Snapshot `json:"snapshot,omitempty"`
}

func (s Snapshot) Validate() error {
	if s.Revision < 0 || len(s.Blocks) > 1024 || len(s.Words) > 32 || len(s.EditedBlockIDs) > 1024 ||
		len(s.MediaOnlyBlockIDs) > 1024 || len(s.PendingUtterances) > MaxPendingUtterances || utf8.RuneCountInString(s.WritingStyle) > 64 {
		return ErrInvalid
	}
	count := utf8.RuneCountInString(s.Transcript)
	seen := map[string]bool{}
	for _, b := range s.Blocks {
		if _, err := uuid.Parse(b.ID); err != nil || len(b.ID) != 36 || seen[b.ID] {
			return ErrInvalid
		}
		seen[b.ID] = true
		count += utf8.RuneCountInString(b.Text)
	}
	for _, id := range s.EditedBlockIDs {
		if !seen[id] {
			return ErrInvalid
		}
	}
	for _, id := range s.MediaOnlyBlockIDs {
		if !seen[id] {
			return ErrInvalid
		}
	}
	sources := map[string]bool{}
	pendingCharacters := 0
	for _, u := range s.PendingUtterances {
		if _, err := uuid.Parse(u.ID); err != nil || sources[u.ID] || strings.TrimSpace(u.Text) == "" ||
			utf8.RuneCountInString(u.Text) > 4096 || utf8.RuneCountInString(u.Speaker) > 160 || utf8.RuneCountInString(u.Person) > 80 ||
			u.StartMilliseconds < 0 || u.EndMilliseconds < u.StartMilliseconds ||
			utf8.RuneCountInString(u.AcousticEmotion) > 512 || math.IsNaN(u.Volume) || math.IsInf(u.Volume, 0) ||
			math.IsNaN(u.SpeechRate) || math.IsInf(u.SpeechRate, 0) {
			return ErrInvalid
		}
		sources[u.ID] = true
		pendingCharacters += utf8.RuneCountInString(u.Text)
	}
	if pendingCharacters > MaxContextCharacters {
		return ErrInvalid
	}
	if count > MaxContextCharacters {
		return ErrInvalid
	}
	wordBytes := 0
	for _, w := range s.Words {
		if w == "" || utf8.RuneCountInString(w) > 32 {
			return ErrInvalid
		}
		wordBytes += len(w)
	}
	// Conservative UTF-8 byte budget: never claim the provider's token budget
	// is equivalent to a character count (bidirectional hotwords: 100 tokens).
	if wordBytes > 96 {
		return ErrInvalid
	}
	return nil
}
func (r Revision) Validate(s Snapshot) error {
	if r.BaseRevision != s.Revision || len(r.Patches) > 1024 || len(r.Passages) > 1024 || len(r.ConsumedSourceIDs) > MaxPendingUtterances || len(r.Questions) > 8 || len(r.Emotions) > 8 {
		return ErrConflict
	}
	known := map[string]bool{}
	lengths := map[string]int{}
	locked := map[string]bool{}
	for _, id := range s.MediaOnlyBlockIDs {
		locked[id] = true
	}
	touched := map[string]bool{}
	for _, b := range s.Blocks {
		known[b.ID] = true
		lengths[b.ID] = utf8.RuneCountInString(b.Text)
	}
	for _, id := range s.EditedBlockIDs {
		locked[id] = true
	}
	count := 0
	for _, p := range r.Patches {
		if _, err := uuid.Parse(p.ID); err != nil || len(p.ID) != 36 || touched[p.ID] || locked[p.ID] {
			return ErrInvalid
		}
		touched[p.ID] = true
		if p.AfterID == "" {
			if !known[p.ID] {
				return ErrInvalid
			}
		} else {
			if known[p.ID] || !known[p.AfterID] {
				return ErrInvalid
			}
			known[p.ID] = true
		}
		count += utf8.RuneCountInString(p.Text)
		lengths[p.ID] = utf8.RuneCountInString(p.Text)
	}
	for _, q := range r.Questions {
		if utf8.RuneCountInString(q) > 300 {
			return ErrInvalid
		}
	}
	sources := map[string]bool{}
	for _, u := range s.PendingUtterances {
		sources[u.ID] = true
	}
	usedSources := map[string]bool{}
	targets := map[string]bool{}
	for _, passage := range r.Passages {
		if !known[passage.BlockID] || passage.ParagraphIndex < 0 || passage.ParagraphIndex > 63 || len(passage.SourceIDs) == 0 || len(passage.SourceIDs) > MaxPendingUtterances {
			return ErrInvalid
		}
		target := passage.BlockID + ":" + fmt.Sprint(passage.ParagraphIndex)
		if targets[target] {
			return ErrInvalid
		}
		targets[target] = true
		for _, id := range passage.SourceIDs {
			if !sources[id] || usedSources[id] {
				return ErrInvalid
			}
			usedSources[id] = true
		}
	}
	consumed := map[string]bool{}
	for _, id := range r.ConsumedSourceIDs {
		if !sources[id] || consumed[id] {
			return ErrInvalid
		}
		consumed[id] = true
	}
	if len(consumed) != len(sources) {
		return ErrInvalid
	}
	for id := range usedSources {
		if !consumed[id] {
			return ErrInvalid
		}
	}
	if len(r.Patches) > 0 && len(s.PendingUtterances) > 0 && len(r.Passages) == 0 {
		return ErrInvalid
	}
	allowedEmotions := map[string]bool{
		"calm": true, "happy": true, "excited": true, "relaxed": true,
		"moved": true, "hopeful": true, "surprised": true, "worried": true,
		"nervous": true, "sad": true, "angry": true, "tired": true,
	}
	if r.OverallEmotion != "" && !allowedEmotions[r.OverallEmotion] {
		return ErrInvalid
	}
	patchText := map[string]string{}
	for _, patch := range r.Patches {
		patchText[patch.ID] = patch.Text
	}
	sourceEvidence := map[string]SourceUtterance{}
	for _, source := range s.PendingUtterances {
		sourceEvidence[source.ID] = source
	}
	emotionSources := map[string]bool{}
	for _, emotion := range r.Emotions {
		source, exists := sourceEvidence[emotion.SourceID]
		text, replaced := patchText[emotion.BlockID]
		if !replaced {
			for _, block := range s.Blocks {
				if block.ID == emotion.BlockID {
					text = block.Text
					break
				}
			}
		}
		linked := false
		for _, passage := range r.Passages {
			if passage.BlockID != emotion.BlockID {
				continue
			}
			for _, id := range passage.SourceIDs {
				if id == emotion.SourceID {
					linked = true
					break
				}
			}
		}
		if !exists || strings.TrimSpace(source.AcousticEmotion) == "" || emotionSources[emotion.SourceID] ||
			!allowedEmotions[emotion.Kind] || strings.TrimSpace(emotion.AnchorText) == "" ||
			utf8.RuneCountInString(emotion.AnchorText) > 80 || strings.Count(text, emotion.AnchorText) != 1 || !linked {
			return ErrInvalid
		}
		emotionSources[emotion.SourceID] = true
	}
	if count > MaxContextCharacters {
		return ErrInvalid
	}
	count = utf8.RuneCountInString(s.Transcript)
	for _, length := range lengths {
		count += length
	}
	if count > MaxContextCharacters {
		return ErrInvalid
	}
	return nil
}

// Month boundaries are anchored to the verified original purchase, clamping
// end-of-month dates without allowing repeated AddDate calls to drift.
func Period(anchor, now time.Time) (time.Time, time.Time) {
	anchor = anchor.UTC()
	now = now.UTC()
	n := (now.Year()-anchor.Year())*12 + int(now.Month()-anchor.Month())
	at := func(offset int) time.Time {
		first := time.Date(anchor.Year(), anchor.Month()+time.Month(offset), 1, anchor.Hour(), anchor.Minute(), anchor.Second(), 0, time.UTC)
		last := first.AddDate(0, 1, -1).Day()
		day := min(anchor.Day(), last)
		return time.Date(first.Year(), first.Month(), day, first.Hour(), first.Minute(), first.Second(), 0, time.UTC)
	}
	start := at(n)
	if start.After(now) {
		n--
		start = at(n)
	}
	return start, at(n + 1)
}
