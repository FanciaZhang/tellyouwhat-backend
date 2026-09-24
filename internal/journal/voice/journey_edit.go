package voice

import (
	"reflect"
	"slices"
	"strings"
)

// Sparse changes preserve everything the speaker did not ask to change.
// Location resolution is deliberately not model-editable: renaming a place
// invalidates its old match and requires the user to confirm the new location.
type JourneyStopUpdate struct {
	StopID            string  `json:"stopID"`
	Expression        *string `json:"expression"`
	TransportToNext   *string `json:"transportToNext"`
	HiddenWhenSharing *bool   `json:"hiddenWhenSharing"`
}

type JourneyStopInsertion struct {
	ID                string `json:"id"`
	Expression        string `json:"expression"`
	TransportToNext   string `json:"transportToNext"`
	HiddenWhenSharing bool   `json:"hiddenWhenSharing"`
}

type JourneyEdit struct {
	ID             string                 `json:"id"`
	BlockID        string                 `json:"blockID"`
	MapID          string                 `json:"mapID"`
	SourceID       string                 `json:"sourceID"`
	Instruction    string                 `json:"instruction"`
	Title          *string                `json:"title"`
	Updates        []JourneyStopUpdate    `json:"updates"`
	Insertions     []JourneyStopInsertion `json:"insertions"`
	RemovedStopIDs []string               `json:"removedStopIDs"`
	StopOrder      []string               `json:"stopOrder"`
}

// Produces an isolated candidate. Source authorization belongs to the revision
// validator; applying a proposal must additionally check the current snapshot.
func applyJourneyEdit(before JourneyContext, command JourneyEdit) (JourneyContext, error) {
	if command.BlockID != before.BlockID || command.MapID != before.MapID ||
		len(command.Updates) > 64 || len(command.Insertions) > 64 || len(command.RemovedStopIDs) > 64 {
		return JourneyContext{}, ErrInvalid
	}
	result := before
	result.Stops = slices.Clone(before.Stops)
	seen := map[string]bool{}
	for _, id := range command.RemovedStopIDs {
		index := slices.IndexFunc(result.Stops, func(s JourneyContextStop) bool { return s.ID == id })
		if seen[id] || index < 0 {
			return JourneyContext{}, ErrInvalid
		}
		seen[id] = true
		result.Stops = slices.Delete(result.Stops, index, index+1)
	}
	for _, patch := range command.Updates {
		index := slices.IndexFunc(result.Stops, func(s JourneyContextStop) bool { return s.ID == patch.StopID })
		if seen[patch.StopID] || index < 0 {
			return JourneyContext{}, ErrInvalid
		}
		seen[patch.StopID] = true
		old := result.Stops[index]
		stop := &result.Stops[index]
		if patch.Expression != nil && *patch.Expression != stop.Expression {
			stop.Expression = *patch.Expression
			stop.Resolution = "needsDetails"
		}
		if patch.TransportToNext != nil {
			stop.TransportToNext = *patch.TransportToNext
		}
		if patch.HiddenWhenSharing != nil {
			stop.HiddenWhenSharing = *patch.HiddenWhenSharing
		}
		if old == *stop && patch.TransportToNext == nil {
			return JourneyContext{}, ErrInvalid
		}
	}
	identities := map[string]bool{strings.ToLower(command.ID): true, strings.ToLower(before.BlockID): true, strings.ToLower(before.MapID): true}
	for _, stop := range before.Stops {
		identities[strings.ToLower(stop.ID)] = true
	}
	for _, insertion := range command.Insertions {
		key := strings.ToLower(insertion.ID)
		if !validID(insertion.ID) || identities[key] {
			return JourneyContext{}, ErrInvalid
		}
		identities[key] = true
		result.Stops = append(result.Stops, JourneyContextStop{ID: insertion.ID, Expression: insertion.Expression,
			Resolution: "needsDetails", TransportToNext: insertion.TransportToNext, HiddenWhenSharing: insertion.HiddenWhenSharing})
	}
	if command.Title != nil {
		result.Title = *command.Title
	}
	if command.StopOrder != nil {
		if len(command.StopOrder) != len(result.Stops) {
			return JourneyContext{}, ErrInvalid
		}
		ordered, used := make([]JourneyContextStop, 0, len(result.Stops)), map[string]bool{}
		for _, id := range command.StopOrder {
			index := slices.IndexFunc(result.Stops, func(s JourneyContextStop) bool { return s.ID == id })
			if used[id] || index < 0 {
				return JourneyContext{}, ErrInvalid
			}
			used[id] = true
			ordered = append(ordered, result.Stops[index])
		}
		result.Stops = ordered
	}
	// Transport belongs to an edge, not just its starting stop. Reordering,
	// deleting or inserting a neighbour invalidates the old edge's transport.
	// An explicit transport update in this command describes the resulting edge.
	for index := range result.Stops {
		stop := &result.Stops[index]
		oldIndex := slices.IndexFunc(before.Stops, func(s JourneyContextStop) bool { return s.ID == stop.ID })
		oldNext, newNext := "", ""
		if oldIndex >= 0 && oldIndex+1 < len(before.Stops) {
			oldNext = before.Stops[oldIndex+1].ID
		}
		if index+1 < len(result.Stops) {
			newNext = result.Stops[index+1].ID
		}
		explicit := slices.ContainsFunc(command.Updates, func(p JourneyStopUpdate) bool { return p.StopID == stop.ID && p.TransportToNext != nil })
		if oldIndex >= 0 && oldNext != newNext && !explicit {
			stop.TransportToNext = "unspecified"
		}
		if newNext == "" {
			if (explicit || oldIndex < 0) && stop.TransportToNext != "unspecified" {
				return JourneyContext{}, ErrInvalid
			}
			stop.TransportToNext = "unspecified"
		}
	}
	if len(result.Stops) == 0 || reflect.DeepEqual(before, result) {
		return JourneyContext{}, ErrInvalid
	}
	return result, nil
}

func validateJourneyEditResult(command JourneyEdit, snapshot Snapshot) error {
	index := slices.IndexFunc(snapshot.JourneyContext, func(c JourneyContext) bool { return c.BlockID == command.BlockID && c.MapID == command.MapID })
	if index < 0 {
		return ErrInvalid
	}
	updated, err := applyJourneyEdit(snapshot.JourneyContext[index], command)
	if err != nil {
		return err
	}
	snapshot.JourneyContext = slices.Clone(snapshot.JourneyContext)
	snapshot.JourneyContext[index] = updated
	return validateJourneyContext(snapshot)
}
