package voice

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// TimelineContext is current editable document state in narrated order.
// Sources must be empty: historical quotes are not new speech evidence.
type TimelineContext struct {
	BlockID    string          `json:"blockID"`
	TimelineID string          `json:"timelineID"`
	Title      string          `json:"title"`
	Events     []TimelineEvent `json:"events"`
}

func validateTimelineContext(s Snapshot) error {
	if len(s.TimelineContext) > 4 {
		return ErrInvalid
	}
	used, blocks, components, owners := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]int{}
	for _, b := range s.Blocks {
		used[b.ID] = true
		blocks[b.ID] = true
	}
	for _, ids := range s.BlockComponents {
		for _, id := range ids {
			used[id] = true
			components[id] = true
			owners[id]++
		}
	}
	for _, table := range s.TableContext {
		for _, c := range table.Columns {
			used[c.ID] = true
		}
		for _, r := range table.Rows {
			used[r.ID] = true
		}
		for _, c := range table.Calculations {
			used[c.ID] = true
		}
	}
	seenTimelines := map[string]bool{}
	refs := func(ids []string, available map[string]bool) bool {
		if len(ids) > 64 {
			return false
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !validID(id) || seen[id] || (available != nil && !available[id]) {
				return false
			}
			seen[id] = true
		}
		return true
	}
	total := 0
	for _, timeline := range s.TimelineContext {
		if !blocks[timeline.BlockID] || !validID(timeline.TimelineID) || blocks[timeline.TimelineID] || owners[timeline.TimelineID] != 1 ||
			seenTimelines[timeline.TimelineID] || !slices.Contains(s.BlockComponents[timeline.BlockID], timeline.TimelineID) ||
			utf8.RuneCountInString(timeline.Title) > 300 || len(timeline.Events) > 64 {
			return ErrInvalid
		}
		for _, table := range s.TableContext {
			if table.TableID == timeline.TimelineID {
				return ErrInvalid
			}
		}
		seenTimelines[timeline.TimelineID] = true
		total += utf8.RuneCountInString(timeline.Title)
		events := map[string]TimelineEvent{}
		for _, e := range timeline.Events {
			if !validID(e.ID) || used[e.ID] || strings.TrimSpace(e.Title) == "" || utf8.RuneCountInString(e.Title) > 300 ||
				utf8.RuneCountInString(e.Detail) > 6000 || utf8.RuneCountInString(e.TimeExpression) > 300 ||
				(e.Intent != "experience" && e.Intent != "plan") || len(e.Sources) != 0 {
				return ErrInvalid
			}
			used[e.ID] = true
			total += utf8.RuneCountInString(e.Title + e.Detail + e.TimeExpression)
			if e.Day != "" {
				day, err := time.Parse("2006-01-02", e.Day)
				if err != nil || day.Year() < 1 || day.Year() > 9999 || day.Format("2006-01-02") != e.Day {
					return ErrInvalid
				}
			}
			switch e.Precision {
			case "day", "unspecified":
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
			if !refs(e.BlockIDs, blocks) || !refs(e.PhotoIDs, components) || !refs(e.PersonIDs, nil) ||
				(e.LocationID != nil && (!validID(*e.LocationID) || !components[*e.LocationID])) {
				return ErrInvalid
			}
			events[e.ID] = e
		}
		if total > 20000 {
			return ErrInvalid
		}
		for _, e := range timeline.Events {
			visited := map[string]bool{e.ID: true}
			for next := e.AfterEventID; next != nil; {
				prior, exists := events[*next]
				if !exists || visited[*next] {
					return ErrInvalid
				}
				visited[*next] = true
				next = prior.AfterEventID
			}
		}
	}
	return nil
}
