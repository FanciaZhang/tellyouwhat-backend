package voice

import (
	"reflect"
	"slices"
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
