package voice

import (
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

// Nullable fields mean unchanged. A supplied time replaces the whole temporal
// value so that precision, approximation and relative ordering stay coherent.
type TimelineEditTime struct {
	Expression   string  `json:"expression"`
	Day          string  `json:"day"`
	Precision    string  `json:"precision"`
	Period       string  `json:"period"`
	Minute       *int    `json:"minute"`
	Approximate  bool    `json:"approximate"`
	AfterEventID *string `json:"afterEventID"`
}

type TimelineEventUpdate struct {
	EventID     string            `json:"eventID"`
	Title       *string           `json:"title"`
	Detail      *string           `json:"detail"`
	Time        *TimelineEditTime `json:"time"`
	Intent      *string           `json:"intent"`
	NeedsReview *bool             `json:"needsReview"`
}

type TimelineEdit struct {
	ID              string                `json:"id"`
	BlockID         string                `json:"blockID"`
	TimelineID      string                `json:"timelineID"`
	SourceID        string                `json:"sourceID"`
	Instruction     string                `json:"instruction"`
	Title           *string               `json:"title"`
	Updates         []TimelineEventUpdate `json:"updates"`
	RemovedEventIDs []string              `json:"removedEventIDs"`
	EventOrder      []string              `json:"eventOrder"`
}

func validateTimelineEdits(r Revision, s Snapshot) error {
	if len(r.TimelineEdits) == 0 {
		return nil
	}
	if len(r.TimelineEdits) > 4 || len(r.MoveCommands)+len(r.MoveResolutions)+len(r.ParagraphCommands)+len(r.ParagraphResolutions) > 0 {
		return ErrInvalid
	}
	used := map[string]bool{}
	for _, b := range s.Blocks {
		used[b.ID] = true
	}
	for _, ids := range s.BlockComponents {
		for _, id := range ids {
			used[id] = true
		}
	}
	for _, t := range s.TimelineContext {
		used[t.TimelineID] = true
		for _, e := range t.Events {
			used[e.ID] = true
		}
	}
	for _, t := range s.TableContext {
		used[t.TableID] = true
		for _, c := range t.Columns {
			used[c.ID] = true
		}
		for _, row := range t.Rows {
			used[row.ID] = true
		}
		for _, c := range t.Calculations {
			used[c.ID] = true
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
	for _, c := range r.TableCreations {
		used[c.ID], used[c.BlockID], used[c.TableID] = true, true, true
		for _, column := range c.Columns {
			used[column.ID] = true
		}
		for _, row := range c.Rows {
			used[row.ID] = true
		}
	}
	for _, c := range r.TableEdits {
		used[c.ID] = true
		for _, p := range c.Patches {
			if p.Row != nil {
				used[p.Row.ID] = true
			}
			if p.Column != nil {
				used[p.Column.ID] = true
			}
			if p.Calculation != nil {
				used[p.Calculation.ID] = true
			}
		}
	}
	for _, c := range r.TimelineCreations {
		used[c.ID], used[c.BlockID], used[c.TimelineID] = true, true, true
		for _, e := range c.Events {
			used[e.ID] = true
		}
	}
	sources, seen := map[string]string{}, map[string]bool{}
	for _, u := range s.PendingUtterances {
		if _, duplicate := sources[u.ID]; duplicate {
			return ErrInvalid
		}
		sources[u.ID] = u.Text
	}
	for _, c := range r.TimelineEdits {
		if !validID(c.ID) || used[c.ID] || seen[c.TimelineID] || !slices.Contains(r.ConsumedSourceIDs, c.SourceID) ||
			strings.TrimSpace(c.Instruction) == "" || utf8.RuneCountInString(c.Instruction) > 500 ||
			strings.Count(sources[c.SourceID], c.Instruction) != 1 {
			return ErrInvalid
		}
		used[c.ID], seen[c.TimelineID] = true, true
		for _, edit := range r.BlockEdits {
			if edit.ID == c.BlockID {
				return ErrInvalid
			}
		}
		for _, edit := range r.Corrections {
			if edit.BlockID == c.BlockID {
				return ErrInvalid
			}
		}
		for _, edit := range r.FormatCommands {
			if edit.BlockID == c.BlockID {
				return ErrInvalid
			}
		}
		for _, edit := range r.TableEdits {
			if edit.BlockID == c.BlockID {
				return ErrInvalid
			}
		}
		for _, resolution := range r.TableResolutions {
			for _, context := range s.TableReceiptContext {
				if resolution.ReceiptID == context.ReceiptID && context.BlockID == c.BlockID {
					return ErrInvalid
				}
			}
		}
		matches, authorized := 0, false
		for _, p := range r.SourcePartitions {
			if p.SourceID != c.SourceID {
				continue
			}
			matches++
			var text strings.Builder
			for _, segment := range p.Segments {
				text.WriteString(segment.Text)
				if segment.Role == "instruction" && segment.Text == c.Instruction && slices.Equal(segment.BlockIDs, []string{c.BlockID}) {
					authorized = true
				}
			}
			if text.String() != sources[c.SourceID] {
				return ErrInvalid
			}
		}
		if matches != 1 || !authorized {
			return ErrInvalid
		}
		if err := validateTimelineEditResult(c, s); err != nil {
			return err
		}
	}
	return nil
}

// Materialization is side-effect free. Evidence authorization and identity
// ownership are separate validation gates before a proposal can be delivered.
func applyTimelineEdit(before TimelineContext, command TimelineEdit) (TimelineContext, error) {
	if command.BlockID != before.BlockID || command.TimelineID != before.TimelineID ||
		len(command.Updates) > 64 || len(command.RemovedEventIDs) > 64 {
		return TimelineContext{}, ErrInvalid
	}
	result := before
	result.Events = slices.Clone(before.Events)
	seen := map[string]bool{}
	for _, id := range command.RemovedEventIDs {
		index := slices.IndexFunc(result.Events, func(e TimelineEvent) bool { return e.ID == id })
		if seen[id] || index < 0 {
			return TimelineContext{}, ErrInvalid
		}
		seen[id] = true
		result.Events = slices.Delete(result.Events, index, index+1)
	}
	for _, patch := range command.Updates {
		index := slices.IndexFunc(result.Events, func(e TimelineEvent) bool { return e.ID == patch.EventID })
		if seen[patch.EventID] || index < 0 {
			return TimelineContext{}, ErrInvalid
		}
		seen[patch.EventID] = true
		old := result.Events[index]
		e := &result.Events[index]
		if patch.Title != nil {
			e.Title = *patch.Title
		}
		if patch.Detail != nil {
			e.Detail = *patch.Detail
		}
		if patch.Intent != nil {
			e.Intent = *patch.Intent
		}
		if patch.NeedsReview != nil {
			e.NeedsReview = *patch.NeedsReview
		}
		if t := patch.Time; t != nil {
			e.TimeExpression, e.Day, e.Precision, e.Period = t.Expression, t.Day, t.Precision, t.Period
			e.Minute, e.Approximate, e.AfterEventID = t.Minute, t.Approximate, t.AfterEventID
		}
		if reflect.DeepEqual(old, *e) {
			return TimelineContext{}, ErrInvalid
		}
	}
	if command.Title != nil {
		result.Title = *command.Title
	}
	if command.EventOrder != nil {
		if len(command.EventOrder) != len(result.Events) {
			return TimelineContext{}, ErrInvalid
		}
		ordered, seenOrder := make([]TimelineEvent, 0, len(result.Events)), map[string]bool{}
		for _, id := range command.EventOrder {
			index := slices.IndexFunc(result.Events, func(e TimelineEvent) bool { return e.ID == id })
			if seenOrder[id] || index < 0 {
				return TimelineContext{}, ErrInvalid
			}
			seenOrder[id] = true
			ordered = append(ordered, result.Events[index])
		}
		result.Events = ordered
	}
	if len(result.Events) == 0 || reflect.DeepEqual(before, result) {
		return TimelineContext{}, ErrInvalid
	}
	return result, nil
}

// Reuses the same temporal/reference constraints as the snapshot context while
// retaining all other timelines for cross-component identity checks.
func validateTimelineEditResult(command TimelineEdit, snapshot Snapshot) error {
	index := slices.IndexFunc(snapshot.TimelineContext, func(t TimelineContext) bool {
		return t.BlockID == command.BlockID && t.TimelineID == command.TimelineID
	})
	if index < 0 {
		return ErrInvalid
	}
	updated, err := applyTimelineEdit(snapshot.TimelineContext[index], command)
	if err != nil {
		return err
	}
	snapshot.TimelineContext = slices.Clone(snapshot.TimelineContext)
	snapshot.TimelineContext[index] = updated
	return validateTimelineContext(snapshot)
}
