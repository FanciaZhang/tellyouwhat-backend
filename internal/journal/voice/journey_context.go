package voice

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// JourneyContext exposes current visit order, never location coordinates,
// addresses or historical speech. IDs survive reordering and repeated visits.
type JourneyContext struct {
	BlockID string               `json:"blockID"`
	MapID   string               `json:"mapID"`
	Title   string               `json:"title"`
	Stops   []JourneyContextStop `json:"stops"`
}

type JourneyContextStop struct {
	ID                string `json:"id"`
	Expression        string `json:"expression"`
	Resolution        string `json:"resolution"`
	TransportToNext   string `json:"transportToNext"`
	HiddenWhenSharing bool   `json:"hiddenWhenSharing"`
}

func validateJourneyContext(s Snapshot) error {
	if len(s.JourneyContext) > 4 {
		return ErrInvalid
	}
	used, blocks, owners := map[string]bool{}, map[string]bool{}, map[string]int{}
	key := strings.ToLower
	for _, b := range s.Blocks {
		used[key(b.ID)] = true
		blocks[key(b.ID)] = true
	}
	for _, ids := range s.BlockComponents {
		for _, id := range ids {
			used[key(id)] = true
			owners[key(id)]++
		}
	}
	reservedMaps := map[string]bool{}
	for _, c := range s.TimelineContext {
		reservedMaps[key(c.TimelineID)] = true
		for _, e := range c.Events {
			used[key(e.ID)] = true
		}
	}
	for _, c := range s.TableContext {
		reservedMaps[key(c.TableID)] = true
		for _, v := range c.Columns {
			used[key(v.ID)] = true
		}
		for _, v := range c.Rows {
			used[key(v.ID)] = true
		}
		for _, v := range c.Calculations {
			used[key(v.ID)] = true
		}
		for _, v := range c.Charts {
			used[key(v.ID)] = true
		}
	}
	total := 0
	for _, c := range s.JourneyContext {
		if !validID(c.BlockID) || !blocks[key(c.BlockID)] || !validID(c.MapID) || blocks[key(c.MapID)] ||
			owners[key(c.MapID)] != 1 || reservedMaps[key(c.MapID)] ||
			!slices.ContainsFunc(s.BlockComponents[c.BlockID], func(id string) bool { return strings.EqualFold(id, c.MapID) }) ||
			strings.TrimSpace(c.Title) == "" || utf8.RuneCountInString(c.Title) > 300 || len(c.Stops) > 64 {
			return ErrInvalid
		}
		reservedMaps[key(c.MapID)] = true
		total += utf8.RuneCountInString(c.Title)
		for _, stop := range c.Stops {
			if !validID(stop.ID) || used[key(stop.ID)] || strings.TrimSpace(stop.Expression) == "" ||
				utf8.RuneCountInString(stop.Expression) > 500 ||
				!slices.Contains([]string{"needsDetails", "needsConfirmation", "confirmed"}, stop.Resolution) ||
				!slices.Contains([]string{"unspecified", "walking", "cycling", "driving", "transit"}, stop.TransportToNext) {
				return ErrInvalid
			}
			used[key(stop.ID)] = true
			total += utf8.RuneCountInString(stop.Expression)
		}
		if total > 20000 {
			return ErrInvalid
		}
	}
	return nil
}
