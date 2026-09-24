package voice

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func journeyEditRevisionFixture() (Snapshot, Revision) {
	s, c := journeyEditFixture()
	c.SourceID = uuid.NewString()
	c.Instruction = "把地图第一站改为山脚"
	name := "山脚"
	c.Updates = []JourneyStopUpdate{{StopID: s.JourneyContext[0].Stops[0].ID, Expression: &name}}
	s.PendingUtterances = []SourceUtterance{{ID: c.SourceID, Text: c.Instruction}}
	s.KnownSourceIDs = append(s.KnownSourceIDs, c.SourceID)
	r := Revision{BaseRevision: s.Revision, TranscriptRevision: 1, JourneyEdits: []JourneyEdit{c},
		ConsumedSourceIDs: []string{c.SourceID}, SourcePartitions: []SourcePartition{{SourceID: c.SourceID,
			Segments: []SourceSegment{{Text: c.Instruction, Role: "instruction", BlockIDs: []string{c.BlockID}}}}}}
	return s, r
}

func TestJourneyEditFullRevisionAuthorization(t *testing.T) {
	s, r := journeyEditRevisionFixture()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Snapshot, *Revision){
		"forged instruction":   func(s *Snapshot, r *Revision) { r.JourneyEdits[0].Instruction = "没有说过" },
		"unconsumed":           func(s *Snapshot, r *Revision) { r.ConsumedSourceIDs = nil },
		"historic instruction": func(s *Snapshot, r *Revision) { s.PendingUtterances = nil },
		"body role":            func(s *Snapshot, r *Revision) { r.SourcePartitions[0].Segments[0].Role = "content" },
		"unrelated target": func(s *Snapshot, r *Revision) {
			r.SourcePartitions[0].Segments[0].BlockIDs = []string{uuid.NewString()}
		},
		"duplicate partition": func(s *Snapshot, r *Revision) { r.SourcePartitions = append(r.SourcePartitions, r.SourcePartitions[0]) },
		"duplicate map": func(s *Snapshot, r *Revision) {
			c := r.JourneyEdits[0]
			c.ID = uuid.NewString()
			r.JourneyEdits = append(r.JourneyEdits, c)
		},
		"identity collision": func(s *Snapshot, r *Revision) {
			r.JourneyEdits[0].ID = strings.ToUpper(s.JourneyContext[0].Stops[0].ID)
		},
		"invented place":     func(s *Snapshot, r *Revision) { value := "机场"; r.JourneyEdits[0].Updates[0].Expression = &value },
		"missing insertions": func(s *Snapshot, r *Revision) { r.JourneyEdits[0].Insertions = nil },
		"mixed body":         func(s *Snapshot, r *Revision) { r.BlockEdits = []BlockEdit{{ID: r.JourneyEdits[0].BlockID}} },
		"mixed formatting": func(s *Snapshot, r *Revision) {
			r.FormatCommands = []FormatCommand{{ID: uuid.NewString(), BlockID: r.JourneyEdits[0].BlockID}}
		},
		"cross proposal id": func(s *Snapshot, r *Revision) { r.JourneyCreations = []JourneyCreation{{ID: r.JourneyEdits[0].ID}} },
	} {
		t.Run(name, func(t *testing.T) {
			s, r := journeyEditRevisionFixture()
			change(&s, &r)
			if r.Validate(s) == nil {
				t.Fatal("accepted unauthorized edit")
			}
		})
	}
}

func TestJourneyEditModelWireContract(t *testing.T) {
	if !slices.Contains(voiceRevisionSchema()["required"].([]string), "journeyEdits") {
		t.Fatal("schema omitted edits")
	}
	for _, mode := range []string{"valid", "missing", "null", "unknown coordinate"} {
		t.Run(mode, func(t *testing.T) {
			s, r := journeyEditRevisionFixture()
			encoded, _ := json.Marshal(r)
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"journeyCreations", "timelineEdits", "timelineCreations", "tableCreations", "tableEdits", "tableResolutions", "formatCommands", "moveCommands", "paragraphCommands", "paragraphResolutions", "moveResolutions", "formatResolutions"} {
				fields[key] = []any{}
			}
			switch mode {
			case "missing":
				delete(fields, "journeyEdits")
			case "null":
				fields["journeyEdits"] = nil
			case "unknown coordinate":
				fields["journeyEdits"].([]any)[0].(map[string]any)["latitude"] = 30
			}
			response, _ := json.Marshal(fields)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var request struct {
					Input string `json:"input"`
				}
				if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				var projected rewriteModelDocument
				if err := json.Unmarshal([]byte(request.Input), &projected); err != nil {
					t.Error(err)
				}
				if !reflect.DeepEqual(projected.JourneyContext, s.JourneyContext) {
					t.Error("lost current map context")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": string(response)}}}}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 20}})
			}))
			defer server.Close()
			model := ArkRewriter{BaseURL: server.URL, APIKey: "test", Model: "test", HTTP: server.Client()}
			result, err := model.Rewrite(context.Background(), s, 1)
			if result.InputTokens != 100 || result.OutputTokens != 20 {
				t.Fatal("lost usage during validation")
			}
			if mode == "valid" {
				if err != nil || !reflect.DeepEqual(result.Revision.JourneyEdits, r.JourneyEdits) {
					t.Fatal("edit wire failed", err)
				}
			} else if !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid response accepted", err)
			}
		})
	}
}

func TestJourneyEditsEnforceCombinedResultBudget(t *testing.T) {
	s := journeyContextFixture()
	s.JourneyContext = nil
	r := Revision{BaseRevision: s.Revision}
	for i := 0; i < 2; i++ {
		id, source := uuid.NewString(), uuid.NewString()
		c := JourneyContext{BlockID: s.Blocks[0].ID, MapID: id, Title: "路线"}
		for j := 0; j < 19; j++ {
			c.Stops = append(c.Stops, JourneyContextStop{ID: uuid.NewString(), Expression: strings.Repeat("字", 500), Resolution: "needsDetails", TransportToNext: "unspecified"})
		}
		s.JourneyContext = append(s.JourneyContext, c)
		s.BlockComponents[c.BlockID] = append(s.BlockComponents[c.BlockID], id)
		text := strings.Repeat("山", 500)
		s.PendingUtterances = append(s.PendingUtterances, SourceUtterance{ID: source, Text: text})
		r.ConsumedSourceIDs = append(r.ConsumedSourceIDs, source)
		r.SourcePartitions = append(r.SourcePartitions, SourcePartition{SourceID: source, Segments: []SourceSegment{{Text: text, Role: "instruction", BlockIDs: []string{c.BlockID}}}})
		r.JourneyEdits = append(r.JourneyEdits, JourneyEdit{ID: uuid.NewString(), BlockID: c.BlockID, MapID: id,
			SourceID: source, Instruction: text, Updates: []JourneyStopUpdate{}, RemovedStopIDs: []string{},
			Insertions: []JourneyStopInsertion{{ID: uuid.NewString(), Expression: text, TransportToNext: "unspecified"}}})
	}
	if err := validateJourneyContext(s); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.JourneyEdits {
		if err := validateJourneyEditResult(c, s); err != nil {
			t.Fatal("each individual result should fit", err)
		}
	}
	if validateJourneyEdits(r, s) == nil {
		t.Fatal("combined map edits exceeded shared budget")
	}
}
