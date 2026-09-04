// Package voice owns the journal-only speech protocol. Provider credentials and
// billing decisions never come from the client.
package voice

import (
	"errors"
	"time"
	"unicode/utf8"
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
type Snapshot struct {
	Revision       int      `json:"revision"`
	Blocks         []Block  `json:"blocks"`
	Transcript     string   `json:"transcript"`
	EditedBlockIDs []string `json:"editedBlockIDs"`
	Words          []string `json:"words"`
}
type Patch struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Empty afterID means replace an existing block; insertions need a new UUID.
	AfterID string `json:"afterID"`
}
type Revision struct {
	BaseRevision       int      `json:"baseRevision"`
	TranscriptRevision int      `json:"transcriptRevision"`
	Patches            []Patch  `json:"patches"`
	Questions          []string `json:"questions"`
}
type Receipt struct {
	SegmentID    string `json:"segmentID"`
	SHA256       string `json:"sha256"`
	Text         string `json:"text"`
	Milliseconds int    `json:"milliseconds"`
}
type Event struct {
	Type                  string    `json:"type"`
	SegmentID             string    `json:"segmentID,omitempty"`
	Text                  string    `json:"text,omitempty"`
	Stable                string    `json:"stable,omitempty"`
	Receipt               *Receipt  `json:"receipt,omitempty"`
	Revision              *Revision `json:"revision,omitempty"`
	RemainingMilliseconds int       `json:"remainingMilliseconds"`
	Code                  string    `json:"code,omitempty"`
}
type Frame struct {
	Type      string    `json:"type"`
	SegmentID string    `json:"segmentID,omitempty"`
	PCM       []byte    `json:"pcm,omitempty"`
	Final     bool      `json:"final,omitempty"`
	Snapshot  *Snapshot `json:"snapshot,omitempty"`
}

func (s Snapshot) Validate() error {
	if s.Revision < 0 || len(s.Blocks) > 1024 || len(s.Words) > 32 || len(s.EditedBlockIDs) > 1024 {
		return ErrInvalid
	}
	count := utf8.RuneCountInString(s.Transcript)
	seen := map[string]bool{}
	for _, b := range s.Blocks {
		if b.ID == "" || seen[b.ID] || len(b.ID) > 64 {
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
	if r.BaseRevision != s.Revision || len(r.Patches) > 1024 || len(r.Questions) > 8 {
		return ErrConflict
	}
	known := map[string]bool{}
	locked := map[string]bool{}
	touched := map[string]bool{}
	for _, b := range s.Blocks {
		known[b.ID] = true
	}
	for _, id := range s.EditedBlockIDs {
		locked[id] = true
	}
	count := 0
	for _, p := range r.Patches {
		if p.ID == "" || len(p.ID) > 64 || touched[p.ID] || locked[p.ID] {
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
	}
	for _, q := range r.Questions {
		if utf8.RuneCountInString(q) > 300 {
			return ErrInvalid
		}
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
