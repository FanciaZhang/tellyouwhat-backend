package voice

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func timelineContextFixture() Snapshot {
	s := tableContextFixture()
	id := uuid.NewString()
	s.BlockComponents[s.Blocks[0].ID] = append(s.BlockComponents[s.Blocks[0].ID], id)
	first := TimelineEvent{ID: uuid.NewString(), Title: "散步", Detail: "沿河走走", TimeExpression: "大概上午", Day: "2026-09-24", Precision: "period", Period: "morning", Approximate: true, Intent: "experience"}
	second := TimelineEvent{ID: uuid.NewString(), Title: "喝茶", TimeExpression: "之后", Precision: "unspecified", AfterEventID: &first.ID, Intent: "plan", NeedsReview: true}
	s.TimelineContext = []TimelineContext{{BlockID: s.Blocks[0].ID, TimelineID: id, Title: "河边行程", Events: []TimelineEvent{first, second}}}
	return s
}

func TestTimelineContextPreservesCurrentStateInModelInput(t *testing.T) {
	s := timelineContextFixture()
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
	if !reflect.DeepEqual(output.TimelineContext, s.TimelineContext) {
		t.Fatal("event state or order changed")
	}
	s.TimelineContext[0].Events = nil
	if err := s.Validate(); err != nil {
		t.Fatal("empty editable timeline rejected", err)
	}
}

func TestTimelineContextRejectsAmbiguousOrInvalidState(t *testing.T) {
	for name, change := range map[string]func(*Snapshot){
		"unknown owner":          func(s *Snapshot) { s.TimelineContext[0].BlockID = uuid.NewString() },
		"wrong component":        func(s *Snapshot) { s.TimelineContext[0].TimelineID = uuid.NewString() },
		"duplicate owner":        func(s *Snapshot) { s.BlockComponents[uuid.NewString()] = []string{s.TimelineContext[0].TimelineID} },
		"duplicate timeline":     func(s *Snapshot) { s.TimelineContext = append(s.TimelineContext, s.TimelineContext[0]) },
		"duplicate event":        func(s *Snapshot) { s.TimelineContext[0].Events[1].ID = s.TimelineContext[0].Events[0].ID },
		"table column collision": func(s *Snapshot) { s.TimelineContext[0].Events[0].ID = s.TableContext[0].Columns[0].ID },
		"table owner collision":  func(s *Snapshot) { s.TimelineContext[0].TimelineID = s.TableContext[0].TableID },
		"block collision":        func(s *Snapshot) { s.TimelineContext[0].Events[0].ID = s.Blocks[0].ID },
		"historical quote": func(s *Snapshot) {
			s.TimelineContext[0].Events[0].Sources = []TableSource{{SourceID: uuid.NewString()}}
		},
		"invalid date":       func(s *Snapshot) { s.TimelineContext[0].Events[0].Day = "2026-02-30" },
		"year zero":          func(s *Snapshot) { s.TimelineContext[0].Events[0].Day = "0000-01-01" },
		"unknown precision":  func(s *Snapshot) { s.TimelineContext[0].Events[0].Precision = "hour" },
		"mixed precision":    func(s *Snapshot) { n := 12; s.TimelineContext[0].Events[0].Minute = &n },
		"cycle":              func(s *Snapshot) { s.TimelineContext[0].Events[0].AfterEventID = &s.TimelineContext[0].Events[1].ID },
		"dangling relation":  func(s *Snapshot) { id := uuid.NewString(); s.TimelineContext[0].Events[1].AfterEventID = &id },
		"unknown photo":      func(s *Snapshot) { s.TimelineContext[0].Events[0].PhotoIDs = []string{uuid.NewString()} },
		"oversized detail":   func(s *Snapshot) { s.TimelineContext[0].Events[0].Detail = strings.Repeat("字", 6001) },
		"too many timelines": func(s *Snapshot) { s.TimelineContext = make([]TimelineContext, 5) },
	} {
		t.Run(name, func(t *testing.T) {
			s := timelineContextFixture()
			change(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("accepted invalid context")
			}
		})
	}
}

func TestTimelineContextEnforcesAggregateBudget(t *testing.T) {
	s := timelineContextFixture()
	s.TimelineContext[0].Events = nil
	for i := 0; i < 4; i++ {
		s.TimelineContext[0].Events = append(s.TimelineContext[0].Events, TimelineEvent{ID: uuid.NewString(), Title: "事件", Detail: strings.Repeat("字", 5000), Precision: "unspecified", Intent: "experience"})
	}
	if err := s.Validate(); err == nil {
		t.Fatal("accepted oversized aggregate")
	}
	s.TimelineContext[0].Events = s.TimelineContext[0].Events[:3]
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}
