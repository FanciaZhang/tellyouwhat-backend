// Package voice owns the journal-only speech protocol. Provider credentials and
// billing decisions never come from the client.
package voice

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const Version = "journal-voice-v1"
const MonthlyMilliseconds = 120 * 60 * 1000
const SessionMilliseconds = 30 * 60 * 1000
const MaxSegmentBytes = 15 * 32000 // PCM16, mono, 16 kHz
const MaxContextCharacters = 60000

var ErrQuota = errors.New("voice_quota_exhausted")
var ErrBusy = errors.New("voice_session_busy")
var ErrConflict = errors.New("voice_revision_conflict")
var ErrInvalid = errors.New("voice_invalid_request")

type Block struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// ManualEdit describes the actual user change; it never grants a permanent lock.
type ManualEdit struct {
	BlockID              string `json:"blockID"`
	Before               string `json:"before"`
	After                string `json:"after"`
	ContextBefore        string `json:"contextBefore"`
	ContextAfter         string `json:"contextAfter"`
	TranscriptOffset     int    `json:"transcriptOffset"`
	PendingEarlierSpeech bool   `json:"pendingEarlierSpeech"`
}

func (e ManualEdit) characters() int {
	return utf8.RuneCountInString(e.Before) + utf8.RuneCountInString(e.After) + utf8.RuneCountInString(e.ContextBefore) + utf8.RuneCountInString(e.ContextAfter)
}

type Snapshot struct {
	rewriteAcknowledged string // server-owned, never decoded from JSON

	RecordingContext  *RecordingContext `json:"recordingContext,omitempty"`
	WritingStyle      WritingStyle      `json:"writingStyle"`
	Revision          int               `json:"revision"`
	Blocks            []Block           `json:"blocks"`
	Transcript        string            `json:"transcript"`
	EditedBlockIDs    []string          `json:"editedBlockIDs"`
	MediaOnlyBlockIDs []string          `json:"mediaOnlyBlockIDs"`
	ManualEdits       []ManualEdit      `json:"manualEdits"`
	Words             []string          `json:"words"`
}
type Patch struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Empty afterID means replace an existing block; insertions need a new UUID.
	AfterID string `json:"afterID"`
}
type EmotionPlacement struct {
	BlockID    string `json:"blockID"`
	AnchorText string `json:"anchorText"`
	SourceID   string `json:"sourceID"`
	Kind       string `json:"kind"`
}

// PassageSource is the authoritative provenance of one resulting paragraph.
// paragraphIndex addresses a paragraph inside a model patch before the App
// splits it into independently editable blocks.
type PassageSource struct {
	BlockID        string   `json:"blockID"`
	ParagraphIndex int      `json:"paragraphIndex"`
	SourceIDs      []string `json:"sourceIDs"`
}
type Revision struct {
	BaseRevision       int                `json:"baseRevision"`
	TranscriptRevision int                `json:"transcriptRevision"`
	Patches            []Patch            `json:"patches"`
	Passages           []PassageSource    `json:"passages"`
	Questions          []string           `json:"questions"`
	Emotions           []EmotionPlacement `json:"emotions"`
	OverallEmotion     string             `json:"overallEmotion"`
}
type Receipt struct {
	Utterances   []StreamUtterance `json:"utterances,omitempty"`
	SegmentID    string            `json:"segmentID"`
	SHA256       string            `json:"sha256"`
	Text         string            `json:"text"`
	Milliseconds int               `json:"milliseconds"`
}
type Event struct {
	Utterances            []StreamUtterance `json:"utterances,omitempty"`
	Type                  string            `json:"type"`
	SegmentID             string            `json:"segmentID,omitempty"`
	Text                  string            `json:"text,omitempty"`
	Stable                string            `json:"stable,omitempty"`
	Receipt               *Receipt          `json:"receipt,omitempty"`
	Revision              *Revision         `json:"revision,omitempty"`
	RemainingMilliseconds int               `json:"remainingMilliseconds"`
	Code                  string            `json:"code,omitempty"`
}
type Frame struct {
	Type      string    `json:"type"`
	SegmentID string    `json:"segmentID,omitempty"`
	PCM       []byte    `json:"pcm,omitempty"`
	Final     bool      `json:"final,omitempty"`
	Snapshot  *Snapshot `json:"snapshot,omitempty"`
}

func (s Snapshot) Validate() error {
	if s.RecordingContext != nil {
		if err := s.RecordingContext.Validate(s.Transcript); err != nil {
			return err
		}
	}
	if s.WritingStyle != "" && !styleID.MatchString(string(s.WritingStyle)) {
		return ErrInvalid
	}
	if s.Revision < 0 || len(s.Blocks) > 1024 || len(s.Words) > 32 || len(s.EditedBlockIDs) > 1024 || len(s.MediaOnlyBlockIDs) > 1024 || len(s.ManualEdits) > 24 {
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
	editCharacters := 0
	for _, edit := range s.ManualEdits {
		if !seen[edit.BlockID] || edit.Before == edit.After || edit.TranscriptOffset < 0 || edit.TranscriptOffset > utf8.RuneCountInString(s.Transcript) {
			return ErrInvalid
		}
		editCharacters += edit.characters()
	}
	if editCharacters > 4096 {
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
	if r.BaseRevision != s.Revision || len(r.Patches) > 1024 || len(r.Passages) > 1024 || len(r.Questions) > 8 || len(r.Emotions) > 8 {
		return ErrConflict
	}
	known := map[string]bool{}
	lengths := map[string]int{}
	locked := map[string]bool{}
	touched := map[string]bool{}
	for _, b := range s.Blocks {
		known[b.ID] = true
		lengths[b.ID] = utf8.RuneCountInString(b.Text)
	}
	// Manual edits are editorial context, not immutable paragraphs. Only
	// media-only compositions are excluded from text replacement.
	for _, id := range s.MediaOnlyBlockIDs {
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
	if err := r.validatePassages(s, known, locked); err != nil {
		return err
	}
	for _, q := range r.Questions {
		if utf8.RuneCountInString(q) > 300 {
			return ErrInvalid
		}
	}
	if err := r.validateEmotions(s); err != nil {
		return err
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

func (r Revision) validatePassages(s Snapshot, known, locked map[string]bool) error {
	if s.RecordingContext == nil {
		if len(r.Passages) != 0 {
			return ErrInvalid
		}
		return nil
	}
	if len(r.Passages) == 0 {
		return ErrInvalid
	}
	texts := make(map[string]string, len(s.Blocks)+len(r.Patches))
	for _, block := range s.Blocks {
		texts[block.ID] = block.Text
	}
	for _, patch := range r.Patches {
		texts[patch.ID] = patch.Text
	}
	positions := make(map[string]int, len(s.RecordingContext.Analysis.Utterances))
	for index, utterance := range s.RecordingContext.Analysis.Utterances {
		positions[utterance.ID] = index
	}
	seen := map[string]bool{}
	sourceCount := 0
	for _, passage := range r.Passages {
		key := passage.BlockID + ":" + strconv.Itoa(passage.ParagraphIndex)
		text, exists := texts[passage.BlockID]
		if !exists || !known[passage.BlockID] || locked[passage.BlockID] || seen[key] ||
			passage.ParagraphIndex < 0 || passage.ParagraphIndex >= paragraphCount(text) || len(passage.SourceIDs) == 0 {
			return ErrInvalid
		}
		seen[key] = true
		previous := -2
		for _, sourceID := range passage.SourceIDs {
			position, exists := positions[sourceID]
			if !exists || (previous >= 0 && position != previous+1) {
				return ErrInvalid
			}
			previous = position
			sourceCount++
			if sourceCount > 10000 {
				return ErrInvalid
			}
		}
	}
	return nil
}

func paragraphCount(text string) int {
	return len(nonEmptyParagraphs(text))
}

func nonEmptyParagraphs(text string) []string {
	paragraphs := make([]string, 0, strings.Count(text, "\n")+1)
	for _, paragraph := range strings.Split(text, "\n") {
		if strings.TrimSpace(paragraph) != "" {
			paragraphs = append(paragraphs, paragraph)
		}
	}
	return paragraphs
}

func uniqueAnchorParagraph(text, anchor string) (int, bool) {
	if anchor == "" || strings.Count(text, anchor) != 1 {
		return 0, false
	}
	for index, paragraph := range nonEmptyParagraphs(text) {
		if strings.Contains(paragraph, anchor) {
			return index, true
		}
	}
	return 0, false
}

var journalEmotionKinds = map[string]bool{
	"calm": true, "happy": true, "excited": true, "relaxed": true,
	"moved": true, "hopeful": true, "surprised": true, "worried": true,
	"nervous": true, "sad": true, "angry": true, "tired": true,
}

func (r Revision) validateEmotions(s Snapshot) error {
	if s.RecordingContext == nil {
		if len(r.Emotions) != 0 || r.OverallEmotion != "" {
			return ErrInvalid
		}
		return nil
	}
	if !journalEmotionKinds[r.OverallEmotion] {
		return ErrInvalid
	}
	texts := make(map[string]string, len(s.Blocks)+len(r.Patches))
	for _, block := range s.Blocks {
		texts[block.ID] = block.Text
	}
	for _, patch := range r.Patches {
		texts[patch.ID] = patch.Text
	}
	evidence := map[string]bool{}
	for _, utterance := range s.RecordingContext.Analysis.Utterances {
		raw := strings.ToLower(strings.TrimSpace(utterance.AcousticEmotion))
		if raw != "" {
			evidence[utterance.ID] = true
		}
	}
	if recordingRequiresEmotionPlacement(s) && len(r.Emotions) == 0 {
		return ErrInvalid
	}
	seenSources := map[string]bool{}
	for _, emotion := range r.Emotions {
		paragraphIndex, uniqueAnchor := uniqueAnchorParagraph(texts[emotion.BlockID], emotion.AnchorText)
		linked := false
		for _, passage := range r.Passages {
			if passage.BlockID == emotion.BlockID && passage.ParagraphIndex == paragraphIndex && slices.Contains(passage.SourceIDs, emotion.SourceID) {
				linked = true
				break
			}
		}
		if !journalEmotionKinds[emotion.Kind] || !evidence[emotion.SourceID] || seenSources[emotion.SourceID] ||
			emotion.AnchorText == "" || utf8.RuneCountInString(emotion.AnchorText) > 80 ||
			!uniqueAnchor || !linked {
			return ErrInvalid
		}
		seenSources[emotion.SourceID] = true
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

var styleID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
