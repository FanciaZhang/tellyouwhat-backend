package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"reflect"
	"strings"
	"testing"
)

func journeyContextFixture() Snapshot {
	s := timelineContextFixture()
	id := uuid.NewString()
	s.BlockComponents[s.Blocks[0].ID] = append(s.BlockComponents[s.Blocks[0].ID], id)
	s.JourneyContext = []JourneyContext{{BlockID: s.Blocks[0].ID, MapID: id, Title: "散步地图", Stops: []JourneyContextStop{
		{ID: uuid.NewString(), Expression: "河边", Resolution: "confirmed", TransportToNext: "walking", HiddenWhenSharing: true},
		{ID: uuid.NewString(), Expression: "河边", Resolution: "needsDetails", TransportToNext: "unspecified"},
	}}}
	return s
}

func TestJourneyContextPreservesCurrentStateInModelInput(t *testing.T) {
	s := journeyContextFixture()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var output rewriteModelDocument
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(output.JourneyContext, s.JourneyContext) {
		t.Fatal("visit identity or state changed")
	}
	projection, _ := json.Marshal(output.JourneyContext)
	for _, forbidden := range []string{"latitude", "longitude", "address", "sourceID", "anchor"} {
		if strings.Contains(string(projection), forbidden) {
			t.Fatal("private field exposed", forbidden)
		}
	}
	s.JourneyContext[0].Stops = nil
	if err := s.Validate(); err != nil {
		t.Fatal("empty editable map rejected", err)
	}
}

func TestJourneyContextRejectsAmbiguousAndUnboundedState(t *testing.T) {
	for name, change := range map[string]func(*Snapshot){
		"unknown owner":       func(s *Snapshot) { s.JourneyContext[0].BlockID = uuid.NewString() },
		"unknown component":   func(s *Snapshot) { s.JourneyContext[0].MapID = uuid.NewString() },
		"duplicate owner":     func(s *Snapshot) { s.BlockComponents[uuid.NewString()] = []string{s.JourneyContext[0].MapID} },
		"duplicate map":       func(s *Snapshot) { s.JourneyContext = append(s.JourneyContext, s.JourneyContext[0]) },
		"duplicate stop":      func(s *Snapshot) { s.JourneyContext[0].Stops[1].ID = s.JourneyContext[0].Stops[0].ID },
		"case collision":      func(s *Snapshot) { s.JourneyContext[0].Stops[1].ID = strings.ToUpper(s.JourneyContext[0].Stops[0].ID) },
		"event collision":     func(s *Snapshot) { s.JourneyContext[0].Stops[0].ID = s.TimelineContext[0].Events[0].ID },
		"column collision":    func(s *Snapshot) { s.JourneyContext[0].Stops[0].ID = s.TableContext[0].Columns[0].ID },
		"component collision": func(s *Snapshot) { s.JourneyContext[0].MapID = s.TimelineContext[0].TimelineID },
		"empty title":         func(s *Snapshot) { s.JourneyContext[0].Title = " " },
		"long title":          func(s *Snapshot) { s.JourneyContext[0].Title = strings.Repeat("字", 301) },
		"long expression":     func(s *Snapshot) { s.JourneyContext[0].Stops[0].Expression = strings.Repeat("字", 501) },
		"resolution":          func(s *Snapshot) { s.JourneyContext[0].Stops[0].Resolution = "invented" },
		"transport":           func(s *Snapshot) { s.JourneyContext[0].Stops[0].TransportToNext = "invented" },
		"map count":           func(s *Snapshot) { s.JourneyContext = make([]JourneyContext, 5) },
		"stop count":          func(s *Snapshot) { s.JourneyContext[0].Stops = make([]JourneyContextStop, 65) },
		"budget": func(s *Snapshot) {
			s.JourneyContext[0].Stops = nil
			for i := 0; i < 41; i++ {
				s.JourneyContext[0].Stops = append(s.JourneyContext[0].Stops, JourneyContextStop{
					ID: uuid.NewString(), Expression: strings.Repeat("字", 500), Resolution: "needsDetails", TransportToNext: "unspecified"})
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := journeyContextFixture()
			change(&s)
			if s.Validate() == nil {
				t.Fatal("accepted invalid map context")
			}
		})
	}
}
