package voice

import (
	"github.com/google/uuid"
	"reflect"
	"strings"
	"testing"
)

func journeyEditFixture() (Snapshot, JourneyEdit) {
	s := journeyContextFixture()
	s.JourneyContext[0].Stops = append(s.JourneyContext[0].Stops, JourneyContextStop{ID: uuid.NewString(), Expression: "酒店", Resolution: "confirmed", TransportToNext: "unspecified"})
	s.JourneyContext[0].Stops[1].TransportToNext = "driving"
	c := s.JourneyContext[0]
	return s, JourneyEdit{ID: uuid.NewString(), BlockID: c.BlockID, MapID: c.MapID,
		Updates: []JourneyStopUpdate{}, Insertions: []JourneyStopInsertion{}, RemovedStopIDs: []string{}}
}

func TestJourneyEditReorderingInvalidatesOnlyChangedEdges(t *testing.T) {
	s, edit := journeyEditFixture()
	before := s.JourneyContext[0]
	edit.StopOrder = []string{before.Stops[0].ID, before.Stops[2].ID, before.Stops[1].ID}
	after, err := applyJourneyEdit(before, edit)
	if err != nil {
		t.Fatal(err)
	}
	for _, stop := range after.Stops {
		if stop.TransportToNext != "unspecified" {
			t.Fatal("carried transport to unrelated edge")
		}
	}
	if !after.Stops[0].HiddenWhenSharing || after.Stops[0].Resolution != "confirmed" {
		t.Fatal("lost private or confirmed state")
	}
	if err := validateJourneyEditResult(edit, s); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, s.JourneyContext[0]) {
		t.Fatal("mutated input")
	}
	walking := "walking"
	edit.Updates = []JourneyStopUpdate{{StopID: before.Stops[0].ID, TransportToNext: &walking}}
	after, err = applyJourneyEdit(before, edit)
	if err != nil || after.Stops[0].TransportToNext != walking {
		t.Fatal("lost explicit new-edge transport", err)
	}
	edit.StopOrder = []string{before.Stops[2].ID, before.Stops[0].ID, before.Stops[1].ID}
	edit.Updates = nil
	after, err = applyJourneyEdit(before, edit)
	if err != nil || after.Stops[1].TransportToNext != "walking" {
		t.Fatal("changed intact edge", err)
	}
}

func TestJourneyEditRenamingRequiresNewLocationConfirmation(t *testing.T) {
	s, edit := journeyEditFixture()
	before := s.JourneyContext[0]
	name := "山脚"
	edit.Updates = []JourneyStopUpdate{{StopID: before.Stops[0].ID, Expression: &name}}
	after, err := applyJourneyEdit(before, edit)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stops[0].Resolution != "needsDetails" || after.Stops[0].Expression != name || !after.Stops[0].HiddenWhenSharing {
		t.Fatal("wrong rename projection")
	}
	if !reflect.DeepEqual(after.Stops[1:], before.Stops[1:]) {
		t.Fatal("touched unrelated stops")
	}
	if err := validateJourneyEditResult(edit, s); err != nil {
		t.Fatal(err)
	}
}

func TestJourneyEditInsertionAndDeletionRetainStableVisits(t *testing.T) {
	s, edit := journeyEditFixture()
	before := s.JourneyContext[0]
	id := uuid.NewString()
	edit.Insertions = []JourneyStopInsertion{{ID: id, Expression: "河边", TransportToNext: "cycling"}}
	edit.RemovedStopIDs = []string{before.Stops[1].ID}
	edit.StopOrder = []string{before.Stops[0].ID, id, before.Stops[2].ID}
	after, err := applyJourneyEdit(before, edit)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stops[1].ID != id || after.Stops[1].Resolution != "needsDetails" || after.Stops[1].TransportToNext != "cycling" {
		t.Fatal("wrong inserted visit")
	}
	if after.Stops[0].TransportToNext != "unspecified" {
		t.Fatal("deleted neighbour retained old route")
	}
	if err := validateJourneyEditResult(edit, s); err != nil {
		t.Fatal(err)
	}
}

func TestJourneyEditRejectsInvalidOrAmbiguousChanges(t *testing.T) {
	for name, change := range map[string]func(*Snapshot, *JourneyEdit){
		"empty":         func(s *Snapshot, c *JourneyEdit) {},
		"unknown owner": func(s *Snapshot, c *JourneyEdit) { c.BlockID = uuid.NewString() },
		"unknown stop": func(s *Snapshot, c *JourneyEdit) {
			n := "别处"
			c.Updates = []JourneyStopUpdate{{StopID: uuid.NewString(), Expression: &n}}
		},
		"duplicate removal": func(s *Snapshot, c *JourneyEdit) {
			id := s.JourneyContext[0].Stops[0].ID
			c.RemovedStopIDs = []string{id, id}
		},
		"delete and update": func(s *Snapshot, c *JourneyEdit) {
			id := s.JourneyContext[0].Stops[0].ID
			n := "别处"
			c.RemovedStopIDs = []string{id}
			c.Updates = []JourneyStopUpdate{{StopID: id, Expression: &n}}
		},
		"incomplete order": func(s *Snapshot, c *JourneyEdit) { c.StopOrder = []string{s.JourneyContext[0].Stops[0].ID} },
		"duplicate order": func(s *Snapshot, c *JourneyEdit) {
			id := s.JourneyContext[0].Stops[0].ID
			c.StopOrder = []string{id, id, id}
		},
		"reuse removed identity": func(s *Snapshot, c *JourneyEdit) {
			id := s.JourneyContext[0].Stops[0].ID
			c.RemovedStopIDs = []string{id}
			c.Insertions = []JourneyStopInsertion{{ID: strings.ToUpper(id), Expression: "别处", TransportToNext: "unspecified"}}
		},
		"cross component identity": func(s *Snapshot, c *JourneyEdit) {
			c.Insertions = []JourneyStopInsertion{{ID: s.TimelineContext[0].Events[0].ID, Expression: "别处", TransportToNext: "unspecified"}}
		},
		"invalid transport": func(s *Snapshot, c *JourneyEdit) {
			n := "teleport"
			c.Updates = []JourneyStopUpdate{{StopID: s.JourneyContext[0].Stops[0].ID, TransportToNext: &n}}
		},
		"final edge": func(s *Snapshot, c *JourneyEdit) {
			n := "walking"
			c.Updates = []JourneyStopUpdate{{StopID: s.JourneyContext[0].Stops[2].ID, TransportToNext: &n}}
		},
		"empty map": func(s *Snapshot, c *JourneyEdit) {
			for _, stop := range s.JourneyContext[0].Stops {
				c.RemovedStopIDs = append(c.RemovedStopIDs, stop.ID)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, c := journeyEditFixture()
			change(&s, &c)
			if validateJourneyEditResult(c, s) == nil {
				t.Fatal("accepted invalid change")
			}
		})
	}
}
