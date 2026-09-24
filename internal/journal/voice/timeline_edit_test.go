package voice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestTimelineEditInsertionPreservesIdentityAndOrder(t *testing.T) {
	s, c := timelineEditFixture()
	before := s.TimelineContext[0]
	c.Updates = nil
	c.Instruction = "再添加一个晚上买菜的事件"
	c.Insertions = []TimelineEventInsertion{{ID: uuid.NewString(), Title: "买菜", Intent: "experience",
		Time: TimelineEditTime{Expression: "晚上", Precision: "period", Period: "evening"}}}
	s.PendingUtterances = []SourceUtterance{{ID: c.SourceID, Text: c.Instruction}}
	r := Revision{BaseRevision: s.Revision, TimelineEdits: []TimelineEdit{c}, ConsumedSourceIDs: []string{c.SourceID},
		SourcePartitions: []SourcePartition{{SourceID: c.SourceID, Segments: []SourceSegment{{
			Text: c.Instruction, Role: "instruction", BlockIDs: []string{c.BlockID}}}}}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Revision
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.TimelineEdits, r.TimelineEdits) || decoded.Validate(s) != nil {
		t.Fatal("insertion wire round trip changed")
	}
	for name, id := range map[string]string{"owner": c.BlockID, "timeline": c.TimelineID, "command": c.ID} {
		t.Run(name, func(t *testing.T) {
			bad := r
			bad.TimelineEdits = slices.Clone(r.TimelineEdits)
			bad.TimelineEdits[0].Insertions = slices.Clone(c.Insertions)
			bad.TimelineEdits[0].Insertions[0].ID = id
			if bad.Validate(s) == nil {
				t.Fatal("accepted reserved identity")
			}
		})
	}
	after, err := applyTimelineEdit(before, c)
	if err != nil || len(after.Events) != len(before.Events)+1 || !reflect.DeepEqual(after.Events[:len(before.Events)], before.Events) {
		t.Fatal("insertion rewrote history", err)
	}
	if len(s.TimelineContext[0].Events) != len(before.Events) {
		t.Fatal("mutated snapshot")
	}
	c.EventOrder = []string{c.Insertions[0].ID, before.Events[0].ID, before.Events[1].ID}
	after, err = applyTimelineEdit(before, c)
	if err != nil || after.Events[0].ID != c.Insertions[0].ID {
		t.Fatal("lost narrated order", err)
	}
	for name, mutate := range map[string]func(*TimelineEdit){
		"existing identity":    func(c *TimelineEdit) { c.Insertions[0].ID = before.Events[0].ID },
		"command identity":     func(c *TimelineEdit) { c.Insertions[0].ID = c.ID },
		"duplicate identity":   func(c *TimelineEdit) { c.Insertions = append(c.Insertions, c.Insertions[0]) },
		"invalid time":         func(c *TimelineEdit) { c.Insertions[0].Time.Period = "yesterday" },
		"dangling predecessor": func(c *TimelineEdit) { id := uuid.NewString(); c.Insertions[0].Time.AfterEventID = &id },
		"new event update":     func(c *TimelineEdit) { c.Updates = []TimelineEventUpdate{{EventID: c.Insertions[0].ID}} },
	} {
		t.Run(name, func(t *testing.T) {
			bad := c
			bad.Insertions = slices.Clone(c.Insertions)
			bad.EventOrder = nil
			mutate(&bad)
			if err := validateTimelineEditResult(bad, s); err == nil {
				t.Fatal("accepted invalid insertion")
			}
		})
	}
}

func TestTimelineEditModelResponseContract(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "missing edits"}[missing], func(t *testing.T) {
			s, c := timelineEditFixture()
			s.PendingUtterances = []SourceUtterance{{ID: c.SourceID, Text: c.Instruction}}
			r := Revision{BaseRevision: s.Revision, TranscriptRevision: 1, TimelineEdits: []TimelineEdit{c},
				ConsumedSourceIDs: []string{c.SourceID}, SourcePartitions: []SourcePartition{{SourceID: c.SourceID,
					Segments: []SourceSegment{{Text: c.Instruction, Role: "instruction", BlockIDs: []string{c.BlockID}}}}}}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"timelineCreations", "tableCreations", "tableEdits", "tableResolutions", "formatCommands", "moveCommands", "paragraphCommands", "paragraphResolutions", "moveResolutions", "formatResolutions"} {
				fields[key] = []any{}
			}
			if missing {
				delete(fields, "timelineEdits")
			}
			body, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			requests := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests <- struct{}{}
				if request.URL.Path != "/responses" {
					t.Error("wrong endpoint")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 20, "output_tokens": 30},
					"output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(body)}}}}})
			}))
			defer server.Close()
			result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "test", Model: "fixture", HTTP: server.Client()}).Rewrite(context.Background(), s, 1)
			select {
			case <-requests:
			default:
				t.Fatal("response decoder was not exercised", err)
			}
			if missing {
				if !errors.Is(err, ErrInvalid) {
					t.Fatal("missing field accepted", err)
				}
			} else if err != nil || !reflect.DeepEqual(result.Revision.TimelineEdits, r.TimelineEdits) {
				t.Fatal("edit response changed", err)
			}
			if result.InputTokens != 20 || result.OutputTokens != 30 {
				t.Fatal("lost metering")
			}
		})
	}
}

func TestTimelineEditRevisionAuthorizesSpeechAndRejectsConflicts(t *testing.T) {
	fixture := func() (Snapshot, Revision) {
		s, c := timelineEditFixture()
		s.PendingUtterances = []SourceUtterance{{ID: c.SourceID, Text: c.Instruction}}
		r := Revision{BaseRevision: s.Revision, TimelineEdits: []TimelineEdit{c}, ConsumedSourceIDs: []string{c.SourceID},
			SourcePartitions: []SourcePartition{{SourceID: c.SourceID, Segments: []SourceSegment{{
				Text: c.Instruction, Role: "instruction", BlockIDs: []string{c.BlockID},
			}}}}}
		return s, r
	}
	s, r := fixture()
	if err := r.Validate(s); err != nil {
		t.Fatal("full revision rejected", err)
	}
	if err := validateTimelineEdits(r, s); err != nil {
		t.Fatal(err)
	}
	if err := validateSourcePartitions(r, s); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(voiceRevisionSchema()["required"].([]string), "timelineEdits") {
		t.Fatal("missing required wire field")
	}
	for name, mutate := range map[string]func(*Revision){
		"not consumed":           func(r *Revision) { r.ConsumedSourceIDs = nil },
		"missing insertions":     func(r *Revision) { r.TimelineEdits[0].Insertions = nil },
		"fabricated instruction": func(r *Revision) { r.TimelineEdits[0].Instruction = "没有说过" },
		"wrong source role":      func(r *Revision) { r.SourcePartitions[0].Segments[0].Role = "content" },
		"wrong source owner":     func(r *Revision) { r.SourcePartitions[0].Segments[0].BlockIDs = []string{uuid.NewString()} },
		"missing partition":      func(r *Revision) { r.SourcePartitions = nil },
		"repeated partition":     func(r *Revision) { r.SourcePartitions = append(r.SourcePartitions, r.SourcePartitions[0]) },
		"duplicate command":      func(r *Revision) { r.TimelineEdits = append(r.TimelineEdits, r.TimelineEdits[0]) },
		"duplicate target": func(r *Revision) {
			c := r.TimelineEdits[0]
			c.ID = uuid.NewString()
			r.TimelineEdits = append(r.TimelineEdits, c)
		},
		"component identity": func(r *Revision) { r.TimelineEdits[0].ID = r.TimelineEdits[0].TimelineID },
		"mixed body edit":    func(r *Revision) { r.BlockEdits = []BlockEdit{{ID: r.TimelineEdits[0].BlockID}} },
		"mixed formatting": func(r *Revision) {
			r.FormatCommands = []FormatCommand{{ID: uuid.NewString(), BlockID: r.TimelineEdits[0].BlockID}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, r := fixture()
			mutate(&r)
			if err := validateTimelineEdits(r, s); err == nil {
				t.Fatal("accepted invalid evidence or conflict")
			}
		})
	}
}

func timelineEditFixture() (Snapshot, TimelineEdit) {
	s := timelineContextFixture()
	t := s.TimelineContext[0]
	c := TimelineEdit{ID: uuid.NewString(), BlockID: t.BlockID, TimelineID: t.TimelineID,
		SourceID: uuid.NewString(), Instruction: "把散步改到下午", RemovedEventIDs: []string{}, Insertions: []TimelineEventInsertion{},
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
