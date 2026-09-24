// Package voice owns the journal-only speech protocol. Provider credentials and
// billing decisions never come from the client.
package voice

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const Version = "journal-voice-v18"
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
	ID    string `json:"id"`
	Text  string `json:"text"`
	Style string `json:"style"`
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
	TimelineContext     []TimelineContext     `json:"timelineContext"`
	TableContext        []TableContext        `json:"tableContext"`
	TableReceiptContext []TableReceiptContext `json:"tableReceiptContext"`
	BlockComponents     map[string][]string   `json:"blockComponents"`
	ParallelColumns     map[string]string     `json:"parallelColumns"`
	Revision            int                   `json:"revision"`
	Blocks              []Block               `json:"blocks"`
	Transcript          string                `json:"transcript"`
	EditedBlockIDs      []string              `json:"editedBlockIDs"`
	MediaOnlyBlockIDs   []string              `json:"mediaOnlyBlockIDs"`
	ActiveBlockIDs      []string              `json:"activeBlockIDs"`
	KnownSourceIDs      []string              `json:"knownSourceIDs"`
	PendingUtterances   []SourceUtterance     `json:"pendingUtterances"`
	SemanticState       SemanticState         `json:"semanticState"`
	Words               []string              `json:"words"`
	WritingStyle        string                `json:"writingStyle"`
	FormatContext       []FormatContext       `json:"formatContext"`
	ParallelGroups      [][]string            `json:"parallelGroups"`
	MoveContext         []MoveContext         `json:"moveContext"`
	ParagraphContext    []ParagraphContext    `json:"paragraphContext"`
}
type BlockEdit struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	AfterID string `json:"afterID"`
	Text    string `json:"text"`
	Style   string `json:"style"`
}
type TextCorrection struct {
	BlockID           string   `json:"blockID"`
	MentionID         string   `json:"mentionID"`
	ExpectedText      string   `json:"expectedText"`
	Replacement       string   `json:"replacement"`
	EvidenceSourceIDs []string `json:"evidenceSourceIDs"`
}
type Revision struct {
	TimelineCreations    []TimelineCreation    `json:"timelineCreations"`
	TableCreations       []TableCreation       `json:"tableCreations"`
	TableEdits           []TableEdit           `json:"tableEdits"`
	TableResolutions     []TableResolution     `json:"tableResolutions"`
	BaseRevision         int                   `json:"baseRevision"`
	TranscriptRevision   int                   `json:"transcriptRevision"`
	BlockEdits           []BlockEdit           `json:"blockEdits"`
	Corrections          []TextCorrection      `json:"corrections"`
	FormatCommands       []FormatCommand       `json:"formatCommands"`
	MoveCommands         []MoveCommand         `json:"moveCommands"`
	ParagraphCommands    []ParagraphCommand    `json:"paragraphCommands"`
	ParagraphResolutions []ParagraphResolution `json:"paragraphResolutions"`
	MoveResolutions      []MoveResolution      `json:"moveResolutions"`
	FormatResolutions    []FormatResolution    `json:"formatResolutions"`
	Passages             []Passage             `json:"passages"`
	ConsumedSourceIDs    []string              `json:"consumedSourceIDs"`
	SourcePartitions     []SourcePartition     `json:"sourcePartitions"`
	SemanticState        SemanticState         `json:"semanticState"`
	Questions            []string              `json:"questions"`
	Emotions             []Emotion             `json:"emotions"`
	OverallEmotion       string                `json:"overallEmotion"`
}
type Passage struct {
	BlockID   string   `json:"blockID"`
	SourceIDs []string `json:"sourceIDs"`
}
type SourcePartition struct {
	SourceID string          `json:"sourceID"`
	Segments []SourceSegment `json:"segments"`
}
type SourceSegment struct {
	Text     string   `json:"text"`
	Role     string   `json:"role"`
	BlockIDs []string `json:"blockIDs"`
}
type SemanticState struct {
	Entities           []Entity         `json:"entities"`
	UnresolvedMentions []Mention        `json:"unresolvedMentions"`
	Outline            []OutlineElement `json:"outline"`
}
type Entity struct {
	ID                string   `json:"id"`
	Kind              string   `json:"kind"`
	Name              string   `json:"name"`
	Reference         string   `json:"reference"`
	Aliases           []string `json:"aliases"`
	EvidenceSourceIDs []string `json:"evidenceSourceIDs"`
}
type Mention struct {
	ID        string   `json:"id"`
	BlockID   string   `json:"blockID"`
	Text      string   `json:"text"`
	EntityID  string   `json:"entityID"`
	SourceIDs []string `json:"sourceIDs"`
}
type OutlineElement struct {
	ID        string   `json:"id"`
	Role      string   `json:"role"`
	Title     string   `json:"title"`
	BlockIDs  []string `json:"blockIDs"`
	SourceIDs []string `json:"sourceIDs"`
	Closed    bool     `json:"closed"`
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
		len(s.MediaOnlyBlockIDs) > 1024 || len(s.ActiveBlockIDs) > 8 || len(s.KnownSourceIDs) > 4096 ||
		len(s.PendingUtterances) > MaxPendingUtterances || utf8.RuneCountInString(s.WritingStyle) > 64 {
		return ErrInvalid
	}
	count := utf8.RuneCountInString(s.Transcript)
	seen := map[string]bool{}
	texts := map[string]string{}
	for _, b := range s.Blocks {
		if !validID(b.ID) || seen[b.ID] || !validBlockStyle(b.Style) {
			return ErrInvalid
		}
		seen[b.ID] = true
		texts[b.ID] = b.Text
		count += utf8.RuneCountInString(b.Text)
	}
	locked := map[string]bool{}
	components := map[string]bool{}
	for id, values := range s.BlockComponents {
		if !seen[id] || len(values) > 12 {
			return ErrInvalid
		}
		for _, value := range values {
			if !validID(value) || components[value] {
				return ErrInvalid
			}
			components[value] = true
		}
	}
	for id, column := range s.ParallelColumns {
		if !seen[id] || strings.TrimSpace(column) == "" || len(column) > 80 {
			return ErrInvalid
		}
	}
	if err := validateParagraphColumns(s); err != nil {
		return err
	}
	for _, id := range s.EditedBlockIDs {
		if !seen[id] {
			return ErrInvalid
		}
		locked[id] = true
	}
	for _, id := range s.MediaOnlyBlockIDs {
		if !seen[id] {
			return ErrInvalid
		}
		locked[id] = true
	}
	active := map[string]bool{}
	for _, id := range s.ActiveBlockIDs {
		if !seen[id] || active[id] || locked[id] {
			return ErrInvalid
		}
		active[id] = true
	}
	allSources := map[string]bool{}
	for _, id := range s.KnownSourceIDs {
		if !validID(id) || allSources[id] {
			return ErrInvalid
		}
		allSources[id] = true
	}
	pending := map[string]bool{}
	pendingCharacters := 0
	for _, u := range s.PendingUtterances {
		if !validID(u.ID) || pending[u.ID] || strings.TrimSpace(u.Text) == "" ||
			utf8.RuneCountInString(u.Text) > 4096 || utf8.RuneCountInString(u.Speaker) > 160 || utf8.RuneCountInString(u.Person) > 80 ||
			u.StartMilliseconds < 0 || u.EndMilliseconds < u.StartMilliseconds ||
			utf8.RuneCountInString(u.AcousticEmotion) > 512 || math.IsNaN(u.Volume) || math.IsInf(u.Volume, 0) ||
			math.IsNaN(u.SpeechRate) || math.IsInf(u.SpeechRate, 0) {
			return ErrInvalid
		}
		pending[u.ID] = true
		allSources[u.ID] = true
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
	if err := validateFormatContext(s.FormatContext, seen); err != nil {
		return err
	}
	if err := validateParallelGroups(s); err != nil {
		return err
	}
	if err := validateMoveContext(s.MoveContext, seen); err != nil {
		return err
	}
	if err := validateParagraphContext(s.ParagraphContext, seen); err != nil {
		return err
	}
	if err := validateTimelineContext(s); err != nil {
		return err
	}
	if err := validateTableContext(s); err != nil {
		return err
	}
	if err := validateTableReceiptContext(s); err != nil {
		return err
	}
	return validateSemanticState(s.SemanticState, texts, allSources, locked)
}

func (r Revision) Validate(s Snapshot) error {
	if r.BaseRevision != s.Revision {
		return ErrConflict
	}
	if len(r.BlockEdits) > 64 || len(r.Corrections) > 32 || len(r.FormatCommands) > 16 || len(r.FormatResolutions) > 8 || len(r.Passages) > 64 ||
		len(r.ConsumedSourceIDs) > MaxPendingUtterances || len(r.Questions) > 8 || len(r.Emotions) > 8 {
		return ErrInvalid
	}
	known := map[string]bool{}
	texts := map[string]string{}
	locked := map[string]bool{}
	for _, id := range s.MediaOnlyBlockIDs {
		locked[id] = true
	}
	for _, b := range s.Blocks {
		known[b.ID] = true
		texts[b.ID] = b.Text
	}
	for _, id := range s.EditedBlockIDs {
		locked[id] = true
	}
	active := map[string]bool{}
	for _, id := range s.ActiveBlockIDs {
		active[id] = true
	}
	if len(active) == 0 {
		for i := len(s.Blocks) - 1; i >= 0; i-- {
			if !locked[s.Blocks[i].ID] {
				active[s.Blocks[i].ID] = true
				break
			}
		}
	}
	touched := map[string]bool{}
	count := 0
	for _, edit := range r.BlockEdits {
		if !validID(edit.ID) || touched[edit.ID] || locked[edit.ID] ||
			!validBlockStyle(edit.Style) || edit.Style == "" || strings.TrimSpace(edit.Text) == "" ||
			strings.ContainsAny(edit.Text, "\r\n") || utf8.RuneCountInString(edit.Text) > MaxRewriteSourceCharacters {
			return ErrInvalid
		}
		touched[edit.ID] = true
		switch edit.Kind {
		case "replace":
			if edit.AfterID != "" || !known[edit.ID] || !active[edit.ID] {
				return ErrInvalid
			}
		case "insert":
			if edit.AfterID == "" || known[edit.ID] || !known[edit.AfterID] {
				return ErrInvalid
			}
			known[edit.ID] = true
		default:
			return ErrInvalid
		}
		texts[edit.ID] = edit.Text
		count += utf8.RuneCountInString(edit.Text)
	}
	for _, q := range r.Questions {
		if utf8.RuneCountInString(q) > 300 {
			return ErrInvalid
		}
	}
	allSources := map[string]bool{}
	for _, id := range s.KnownSourceIDs {
		allSources[id] = true
	}
	sources := map[string]bool{}
	for _, u := range s.PendingUtterances {
		sources[u.ID] = true
		allSources[u.ID] = true
	}
	mentions := map[string]Mention{}
	for _, mention := range s.SemanticState.UnresolvedMentions {
		mentions[mention.ID] = mention
	}
	correctedMentions := map[string]bool{}
	for _, correction := range r.Corrections {
		mention, exists := mentions[correction.MentionID]
		if !exists || correctedMentions[correction.MentionID] || correction.BlockID != mention.BlockID ||
			correction.ExpectedText != mention.Text || touched[correction.BlockID] || locked[correction.BlockID] ||
			strings.TrimSpace(correction.Replacement) == "" || correction.Replacement == correction.ExpectedText ||
			utf8.RuneCountInString(correction.ExpectedText) > 80 || utf8.RuneCountInString(correction.Replacement) > 80 ||
			strings.ContainsAny(correction.Replacement, "\r\n") || strings.Count(texts[correction.BlockID], correction.ExpectedText) != 1 ||
			len(correction.EvidenceSourceIDs) == 0 || len(correction.EvidenceSourceIDs) > 16 {
			return ErrInvalid
		}
		seenEvidence := map[string]bool{}
		for _, id := range correction.EvidenceSourceIDs {
			if !sources[id] || seenEvidence[id] {
				return ErrInvalid
			}
			seenEvidence[id] = true
		}
		texts[correction.BlockID] = strings.Replace(texts[correction.BlockID], correction.ExpectedText, correction.Replacement, 1)
		correctedMentions[correction.MentionID] = true
	}
	usedSources := map[string]bool{}
	if err := validateTimelineCreations(r.TimelineCreations, r, s); err != nil {
		return err
	}
	if err := validateTables(r, s); err != nil {
		return err
	}
	if err := validateTableEdits(r, s); err != nil {
		return err
	}
	if err := validateTableResolutions(r, s); err != nil {
		return err
	}
	if err := validateParagraphResolutions(r, s, touched); err != nil {
		return err
	}
	if err := validateParagraphs(r, s, touched); err != nil {
		return err
	}
	if err := validateMoves(r, s, touched); err != nil {
		return err
	}
	if err := validateMoveResolutions(r, s, touched); err != nil {
		return err
	}
	if err := validateFormatResolutions(r, s, touched); err != nil {
		return err
	}
	commandIDs := map[string]bool{}
	for _, command := range r.FormatCommands {
		if !validID(command.ID) || commandIDs[command.ID] || !known[command.BlockID] ||
			!sources[command.SourceID] || !validFormatMark(command.Mark) ||
			command.Anchor.Quote == "" || utf8.RuneCountInString(command.Anchor.Quote) > 6000 ||
			utf8.RuneCountInString(command.Anchor.Prefix) > 80 || utf8.RuneCountInString(command.Anchor.Suffix) > 80 ||
			command.Instruction == "" || utf8.RuneCountInString(command.Instruction) > 500 {
			return ErrInvalid
		}
		for _, id := range s.MediaOnlyBlockIDs {
			if command.BlockID == id {
				return ErrInvalid
			}
		}
		if paragraphFormatMark(command.Mark) && (!command.Enabled || command.Anchor.Quote != texts[command.BlockID] ||
			command.Anchor.Prefix != "" || command.Anchor.Suffix != "") {
			return ErrInvalid
		}
		foundEvidence := false
		for _, source := range s.PendingUtterances {
			if source.ID == command.SourceID && strings.Count(source.Text, command.Instruction) == 1 {
				foundEvidence = true
			}
		}
		if !foundEvidence {
			return ErrInvalid
		}
		commandIDs[command.ID] = true
	}
	targets := map[string]bool{}
	for _, passage := range r.Passages {
		if !known[passage.BlockID] || len(passage.SourceIDs) == 0 || len(passage.SourceIDs) > MaxPendingUtterances || targets[passage.BlockID] {
			return ErrInvalid
		}
		targets[passage.BlockID] = true
		passageSources := map[string]bool{}
		for _, id := range passage.SourceIDs {
			if !sources[id] || passageSources[id] {
				return ErrInvalid
			}
			passageSources[id] = true
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
	if err := validateSourcePartitions(r, s); err != nil {
		return err
	}
	for id := range usedSources {
		if !consumed[id] {
			return ErrInvalid
		}
	}
	if len(r.BlockEdits) > 0 && len(s.PendingUtterances) > 0 && len(r.Passages) == 0 {
		return ErrInvalid
	}
	for _, edit := range r.BlockEdits {
		if !targets[edit.ID] {
			return ErrInvalid
		}
	}
	for id := range correctedMentions {
		for _, unresolved := range r.SemanticState.UnresolvedMentions {
			if unresolved.ID == id {
				return ErrInvalid
			}
		}
	}
	if err := validateSemanticState(r.SemanticState, texts, allSources, locked); err != nil {
		return err
	}
	allowedEmotions := map[string]bool{
		"calm": true, "happy": true, "excited": true, "relaxed": true,
		"moved": true, "hopeful": true, "surprised": true, "worried": true,
		"nervous": true, "sad": true, "angry": true, "tired": true,
	}
	if r.OverallEmotion != "" && !allowedEmotions[r.OverallEmotion] {
		return ErrInvalid
	}
	sourceEvidence := map[string]SourceUtterance{}
	for _, source := range s.PendingUtterances {
		sourceEvidence[source.ID] = source
	}
	emotionSources := map[string]bool{}
	for _, emotion := range r.Emotions {
		source, exists := sourceEvidence[emotion.SourceID]
		text := texts[emotion.BlockID]
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
	for _, text := range texts {
		count += utf8.RuneCountInString(text)
	}
	if count > MaxContextCharacters {
		return ErrInvalid
	}
	return nil
}

func validID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil && len(value) == 36
}

func validBlockStyle(value string) bool {
	switch value {
	case "", "body", "heading1", "heading2", "heading3", "unorderedListItem", "orderedListItem", "checklistItem", "completedChecklistItem":
		return true
	default:
		return false
	}
}

func validateSemanticState(state SemanticState, texts map[string]string, sources, locked map[string]bool) error {
	if len(state.Entities) > 32 || len(state.UnresolvedMentions) > 24 || len(state.Outline) > 32 {
		return ErrInvalid
	}
	semanticCharacters := 0
	entities := map[string]bool{}
	for _, entity := range state.Entities {
		if !validID(entity.ID) || entities[entity.ID] || strings.TrimSpace(entity.Name) == "" ||
			utf8.RuneCountInString(entity.Name) > 80 || len(entity.Aliases) > 6 || len(entity.EvidenceSourceIDs) > 16 ||
			!validEntityKind(entity.Kind) || !validEntityReference(entity.Reference) {
			return ErrInvalid
		}
		semanticCharacters += utf8.RuneCountInString(entity.Name)
		entities[entity.ID] = true
		aliases := map[string]bool{}
		for _, alias := range entity.Aliases {
			if strings.TrimSpace(alias) == "" || utf8.RuneCountInString(alias) > 80 || aliases[alias] {
				return ErrInvalid
			}
			aliases[alias] = true
			semanticCharacters += utf8.RuneCountInString(alias)
		}
		if !validSourceIDs(entity.EvidenceSourceIDs, sources, 16) {
			return ErrInvalid
		}
	}
	mentions := map[string]bool{}
	for _, mention := range state.UnresolvedMentions {
		if !validID(mention.ID) || mentions[mention.ID] || !validID(mention.BlockID) ||
			locked[mention.BlockID] || strings.TrimSpace(mention.Text) == "" || utf8.RuneCountInString(mention.Text) > 80 ||
			strings.Count(texts[mention.BlockID], mention.Text) != 1 ||
			(mention.EntityID != "" && !entities[mention.EntityID]) ||
			!validSourceIDs(mention.SourceIDs, sources, 16) {
			return ErrInvalid
		}
		mentions[mention.ID] = true
		semanticCharacters += utf8.RuneCountInString(mention.Text)
	}
	outline := map[string]bool{}
	for _, element := range state.Outline {
		if !validID(element.ID) || outline[element.ID] || !validOutlineRole(element.Role) ||
			utf8.RuneCountInString(element.Title) > 120 || len(element.BlockIDs) == 0 || len(element.BlockIDs) > 8 ||
			len(element.SourceIDs) > 32 {
			return ErrInvalid
		}
		outline[element.ID] = true
		blocks := map[string]bool{}
		for _, id := range element.BlockIDs {
			if !validID(id) || texts[id] == "" || blocks[id] {
				return ErrInvalid
			}
			blocks[id] = true
		}
		if !validSourceIDs(element.SourceIDs, sources, 32) {
			return ErrInvalid
		}
		semanticCharacters += utf8.RuneCountInString(element.Title)
	}
	if semanticCharacters > MaxRewriteContextCharacters {
		return ErrInvalid
	}
	return nil
}

func validSourceIDs(ids []string, sources map[string]bool, maximum int) bool {
	if len(ids) > maximum {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !validID(id) || !sources[id] || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func validEntityKind(value string) bool {
	switch value {
	case "person", "animal", "place", "object", "organization", "event", "unknown":
		return true
	default:
		return false
	}
}

func validEntityReference(value string) bool {
	switch value {
	case "unknown", "he", "she", "it", "they", "femaleThey", "nonhumanThey":
		return true
	default:
		return false
	}
}

func validOutlineRole(value string) bool {
	switch value {
	case "background", "topic", "point", "summary", "conclusion":
		return true
	default:
		return false
	}
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
