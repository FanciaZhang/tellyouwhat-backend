package voice

import (
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
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

func validateJourneyEdits(r Revision, s Snapshot) error {
	if len(r.JourneyEdits) == 0 {
		return nil
	}
	if len(r.JourneyEdits) > 4 || len(r.MoveCommands)+len(r.MoveResolutions)+len(r.ParagraphCommands)+len(r.ParagraphResolutions) > 0 {
		return ErrInvalid
	}
	if err := validateJourneyContext(s); err != nil {
		return err
	}
	used, touched := map[string]bool{}, map[string]bool{}
	claim := func(id string) bool {
		key := strings.ToLower(id)
		if !validID(id) || used[key] {
			return false
		}
		used[key] = true
		return true
	}
	reserve := func(ids ...string) {
		for _, id := range ids {
			used[strings.ToLower(id)] = true
		}
	}
	for _, b := range s.Blocks {
		reserve(b.ID)
	}
	for _, ids := range s.BlockComponents {
		reserve(ids...)
	}
	for _, c := range s.JourneyContext {
		for _, stop := range c.Stops {
			reserve(stop.ID)
		}
	}
	for _, c := range s.TimelineContext {
		for _, e := range c.Events {
			reserve(e.ID)
		}
	}
	for _, c := range s.TableContext {
		for _, v := range c.Rows {
			reserve(v.ID)
		}
		for _, v := range c.Columns {
			reserve(v.ID)
		}
		for _, v := range c.Calculations {
			reserve(v.ID)
		}
	}
	for _, c := range s.FormatContext {
		reserve(c.ReceiptID)
	}
	for _, c := range s.MoveContext {
		reserve(c.ReceiptID)
	}
	for _, c := range s.ParagraphContext {
		reserve(c.ReceiptID)
	}
	for _, c := range s.TableReceiptContext {
		reserve(c.ReceiptID)
	}
	for _, c := range r.BlockEdits {
		reserve(c.ID)
		touched[c.ID] = true
	}
	for _, c := range r.Corrections {
		touched[c.BlockID] = true
	}
	for _, c := range r.FormatCommands {
		reserve(c.ID)
		touched[c.BlockID] = true
	}
	for _, c := range r.FormatResolutions {
		reserve(c.ID)
		for _, context := range s.FormatContext {
			if context.ReceiptID == c.ReceiptID {
				touched[context.BlockID] = true
			}
		}
	}
	for _, c := range r.TableResolutions {
		reserve(c.ID)
		for _, context := range s.TableReceiptContext {
			if context.ReceiptID == c.ReceiptID {
				touched[context.BlockID] = true
			}
		}
	}
	for _, c := range r.TableCreations {
		reserve(c.ID, c.BlockID, c.TableID)
		for _, v := range c.Rows {
			reserve(v.ID)
		}
		for _, v := range c.Columns {
			reserve(v.ID)
		}
	}
	for _, c := range r.TableEdits {
		reserve(c.ID)
		touched[c.BlockID] = true
		for _, p := range c.Patches {
			if p.Row != nil {
				reserve(p.Row.ID)
			}
			if p.Column != nil {
				reserve(p.Column.ID)
			}
			if p.Calculation != nil {
				reserve(p.Calculation.ID)
			}
		}
	}
	for _, c := range r.TimelineCreations {
		reserve(c.ID, c.BlockID, c.TimelineID)
		for _, e := range c.Events {
			reserve(e.ID)
		}
	}
	for _, c := range r.TimelineEdits {
		reserve(c.ID)
		touched[c.BlockID] = true
		for _, e := range c.Insertions {
			reserve(e.ID)
		}
	}
	for _, c := range r.JourneyCreations {
		reserve(c.ID, c.BlockID, c.MapID)
		for _, v := range c.Visits {
			reserve(v.ID)
		}
	}
	sources := map[string]string{}
	for _, u := range s.PendingUtterances {
		if _, ok := sources[u.ID]; ok {
			return ErrInvalid
		}
		sources[u.ID] = u.Text
	}
	seen := map[string]bool{}
	for _, c := range r.JourneyEdits {
		if !claim(c.ID) || seen[c.MapID] || touched[c.BlockID] || c.Updates == nil || c.Insertions == nil || c.RemovedStopIDs == nil ||
			!slices.Contains(r.ConsumedSourceIDs, c.SourceID) || strings.TrimSpace(c.Instruction) == "" ||
			utf8.RuneCountInString(c.Instruction) > 500 || strings.Count(sources[c.SourceID], c.Instruction) != 1 {
			return ErrInvalid
		}
		seen[c.MapID] = true
		for _, v := range c.Insertions {
			if !claim(v.ID) || !strings.Contains(c.Instruction, v.Expression) {
				return ErrInvalid
			}
		}
		for _, p := range c.Updates {
			if p.Expression != nil && !strings.Contains(c.Instruction, *p.Expression) {
				return ErrInvalid
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
		if err := validateJourneyEditResult(c, s); err != nil {
			return err
		}
	}
	return nil
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
