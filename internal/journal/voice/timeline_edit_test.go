package voice

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func timelineEditFixture() (Snapshot, TimelineEdit) {
	s := timelineContextFixture()
	t := s.TimelineContext[0]
	c := TimelineEdit{ID: uuid.NewString(), BlockID: t.BlockID, TimelineID: t.TimelineID,
		SourceID: uuid.NewString(), Instruction: "把散步改到下午", RemovedEventIDs: []string{},
		Updates: []TimelineEventUpdate{{EventID: t.Events[0].ID,
			Time: &TimelineEditTime{Expression: "下午", Day: "2026-09-24", Precision: "period", Period: "afternoon"}}}}
	return s, c
}

func TestTimelineEditMaterializesOnlyChangedFields(t *testing.T) {
	s, c := timelineEditFixture()
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TimelineEdit
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, decoded) {
		t.Fatal("wire changed")
	}
	before := s.TimelineContext[0]
	after, err := applyTimelineEdit(before, decoded)
	if err != nil {
		t.Fatal(err)
	}
	if after.Events[0].Detail != before.Events[0].Detail || after.Events[0].ID != before.Events[0].ID ||
		!reflect.DeepEqual(after.Events[1], before.Events[1]) {
		t.Fatal("untouched content changed")
	}
	if before.Events[0].Period != "morning" || after.Events[0].Period != "afternoon" {
		t.Fatal("mutated input or missed edit")
	}
	if err := validateTimelineEditResult(c, s); err != nil {
		t.Fatal(err)
	}
}

func TestTimelineEditRejectsInvalidSparseChanges(t *testing.T) {
	for name, mutate := range map[string]func(*TimelineEdit){
		"wrong owner":          func(c *TimelineEdit) { c.BlockID = uuid.NewString() },
		"unknown event":        func(c *TimelineEdit) { c.Updates[0].EventID = uuid.NewString() },
		"duplicate update":     func(c *TimelineEdit) { c.Updates = append(c.Updates, c.Updates[0]) },
		"update removed":       func(c *TimelineEdit) { c.RemovedEventIDs = []string{c.Updates[0].EventID} },
		"dangling predecessor": func(c *TimelineEdit) { c.RemovedEventIDs = []string{c.Updates[0].EventID}; c.Updates = nil },
		"invalid precision":    func(c *TimelineEdit) { c.Updates[0].Time.Precision = "exact" },
		"invalid date":         func(c *TimelineEdit) { c.Updates[0].Time.Day = "2026-02-30" },
		"conflicting time":     func(c *TimelineEdit) { v := 50; c.Updates[0].Time.Minute = &v },
		"self reference":       func(c *TimelineEdit) { c.Updates[0].Time.AfterEventID = &c.Updates[0].EventID },
		"empty patch":          func(c *TimelineEdit) { c.Updates[0].Time = nil },
		"no operation":         func(c *TimelineEdit) { c.Updates = nil },
		"bad order":            func(c *TimelineEdit) { c.EventOrder = []string{c.Updates[0].EventID} },
	} {
		t.Run(name, func(t *testing.T) {
			s, c := timelineEditFixture()
			mutate(&c)
			if err := validateTimelineEditResult(c, s); err == nil {
				t.Fatal("accepted invalid change")
			}
		})
	}
}

func TestTimelineEditReordersAndRemovesWithoutRewritingEvents(t *testing.T) {
	s, c := timelineEditFixture()
	before := s.TimelineContext[0]
	c.Updates = nil
	c.EventOrder = []string{before.Events[1].ID, before.Events[0].ID}
	after, err := applyTimelineEdit(before, c)
	if err != nil || !reflect.DeepEqual(after.Events[0], before.Events[1]) {
		t.Fatal("order failed", err)
	}
	if err := validateTimelineEditResult(c, s); err != nil {
		t.Fatal(err)
	}
	c.EventOrder = nil
	c.RemovedEventIDs = []string{before.Events[1].ID}
	if err := validateTimelineEditResult(c, s); err != nil {
		t.Fatal(err)
	}
	c.RemovedEventIDs = append(c.RemovedEventIDs, before.Events[0].ID)
	if err := validateTimelineEditResult(c, s); err == nil {
		t.Fatal("removed entire timeline")
	}
}
