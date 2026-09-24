package voice

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// The archive identity is deliberately supplied by the client, not the model.
type TimelineEvent struct {
	ID             string        `json:"id"`
	Title          string        `json:"title"`
	Detail         string        `json:"detail"`
	TimeExpression string        `json:"timeExpression"`
	Day            string        `json:"day"`
	Precision      string        `json:"precision"`
	Period         string        `json:"period"`
	Minute         *int          `json:"minute"`
	Approximate    bool          `json:"approximate"`
	AfterEventID   *string       `json:"afterEventID"`
	Intent         string        `json:"intent"`
	NeedsReview    bool          `json:"needsReview"`
	Sources        []TableSource `json:"sources"`
	BlockIDs       []string      `json:"blockIDs"`
	PhotoIDs       []string      `json:"photoIDs"`
	LocationID     *string       `json:"locationID"`
	PersonIDs      []string      `json:"personIDs"`
}

type TimelineCreation struct {
	ID          string          `json:"id"`
	BlockID     string          `json:"blockID"`
	TimelineID  string          `json:"timelineID"`
	AfterID     *string         `json:"afterID"`
	SourceID    string          `json:"sourceID"`
	Instruction string          `json:"instruction"`
	Title       string          `json:"title"`
	Events      []TimelineEvent `json:"events"`
}

// Validate against the actual consumed speech, not a model's reconstructed
// transcript. References are also revalidated against typed objects on device.
func validateTimelineCreations(commands []TimelineCreation, r Revision, s Snapshot) error {
	if len(commands) > 4 {
		return ErrInvalid
	}
	if len(commands) == 0 {
		return nil
	}
	if len(r.MoveCommands)+len(r.MoveResolutions)+len(r.ParagraphCommands)+len(r.ParagraphResolutions) > 0 {
		return ErrInvalid
	}
	used, blocks, components := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, b := range s.Blocks {
		used[b.ID] = true
		blocks[b.ID] = true
	}
	for _, ids := range s.BlockComponents {
		for _, id := range ids {
			used[id] = true
			components[id] = true
		}
	}
	for _, c := range r.BlockEdits {
		used[c.ID] = true
	}
	for _, c := range r.FormatCommands {
		used[c.ID] = true
	}
	for _, c := range r.FormatResolutions {
		used[c.ID] = true
	}
	for _, c := range r.TableResolutions {
		used[c.ID] = true
	}
	for _, c := range r.TableEdits {
		used[c.ID] = true
		for _, patch := range c.Patches {
			if patch.Row != nil {
				used[patch.Row.ID] = true
			}
			if patch.Column != nil {
				used[patch.Column.ID] = true
			}
			if patch.Calculation != nil {
				used[patch.Calculation.ID] = true
			}
		}
	}
	for _, c := range s.FormatContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.MoveContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.ParagraphContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.TableReceiptContext {
		used[c.ReceiptID] = true
	}
	for _, table := range s.TableContext {
		used[table.TableID] = true
		for _, column := range table.Columns {
			used[column.ID] = true
		}
		for _, row := range table.Rows {
			used[row.ID] = true
		}
		for _, calculation := range table.Calculations {
			used[calculation.ID] = true
		}
	}
	for _, timeline := range s.TimelineContext {
		used[timeline.TimelineID] = true
		for _, event := range timeline.Events {
			used[event.ID] = true
		}
	}
	for _, c := range r.TableCreations {
		used[c.ID] = true
		used[c.BlockID] = true
		used[c.TableID] = true
		for _, col := range c.Columns {
			used[col.ID] = true
		}
		for _, row := range c.Rows {
			used[row.ID] = true
		}
	}
	claim := func(id string) bool {
		if !validID(id) || used[id] {
			return false
		}
		used[id] = true
		return true
	}
	sources := map[string]string{}
	for _, u := range s.PendingUtterances {
		if slices.Contains(r.ConsumedSourceIDs, u.ID) {
			sources[u.ID] = u.Text
		}
	}
	linked := func(source TableSource, block, role string) bool {
		text, exists := sources[source.SourceID]
		if !exists {
			return false
		}
		start, end, ok := tableAnchorRange(text, source.Anchor)
		if !ok {
			return false
		}
		matches, found := 0, false
		for _, p := range r.SourcePartitions {
			if p.SourceID != source.SourceID {
				continue
			}
			matches++
			joined, offset := "", 0
			for _, segment := range p.Segments {
				next := offset + len(segment.Text)
				if segment.Role == role && slices.Equal(segment.BlockIDs, []string{block}) && start >= offset && end <= next && (role != "instruction" || (start == offset && end == next)) {
					found = true
				}
				joined += segment.Text
				offset = next
			}
			if joined != text {
				return false
			}
		}
		return matches == 1 && found
	}
	refs := func(ids []string, known map[string]bool) bool {
		if len(ids) > 64 {
			return false
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !validID(id) || seen[id] || (known != nil && !known[id]) {
				return false
			}
			seen[id] = true
		}
		return true
	}
	total := 0
	for _, c := range commands {
		if !claim(c.ID) || !claim(c.BlockID) || !claim(c.TimelineID) || (c.AfterID != nil && !blocks[*c.AfterID]) || strings.TrimSpace(c.Title) == "" || utf8.RuneCountInString(c.Title) > 300 || utf8.RuneCountInString(c.Instruction) > 500 || len(c.Events) == 0 || len(c.Events) > 64 || !linked(TableSource{SourceID: c.SourceID, Anchor: TextAnchor{Quote: c.Instruction}}, c.BlockID, "instruction") {
			return ErrInvalid
		}
		for _, e := range c.Events {
			if !claim(e.ID) || strings.TrimSpace(e.Title) == "" || utf8.RuneCountInString(e.Title) > 300 || utf8.RuneCountInString(e.Detail) > 6000 || utf8.RuneCountInString(e.TimeExpression) > 300 || (e.Intent != "experience" && e.Intent != "plan") || len(e.Sources) == 0 || len(e.Sources) > 16 {
				return ErrInvalid
			}
			total += utf8.RuneCountInString(e.Title + e.Detail)
			if total > 20000 {
				return ErrInvalid
			}
			if e.Day != "" {
				day, err := time.Parse("2006-01-02", e.Day)
				if err != nil || day.Format("2006-01-02") != e.Day {
					return ErrInvalid
				}
			}
			switch e.Precision {
			case "unspecified":
				if e.Period != "" || e.Minute != nil {
					return ErrInvalid
				}
			case "day":
				if e.Period != "" || e.Minute != nil {
					return ErrInvalid
				}
			case "period":
				if e.Minute != nil || !slices.Contains([]string{"earlyMorning", "morning", "noon", "afternoon", "evening", "night"}, e.Period) {
					return ErrInvalid
				}
			case "minute":
				if e.Minute == nil || *e.Minute < 0 || *e.Minute >= 1440 || e.Period != "" {
					return ErrInvalid
				}
			default:
				return ErrInvalid
			}
			if !refs(e.BlockIDs, blocks) || !refs(e.PhotoIDs, components) || !refs(e.PersonIDs, nil) || (e.LocationID != nil && !components[*e.LocationID]) {
				return ErrInvalid
			}
			seen := map[TableSource]bool{}
			for _, source := range e.Sources {
				if seen[source] || !linked(source, c.BlockID, "content") {
					return ErrInvalid
				}
				seen[source] = true
			}
		}
		if !validTimelineOrder(c.Events) {
			return ErrInvalid
		}
	}
	return nil
}
