package voice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestJourneyModelWireContract(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "null", "unknown geographic field"} {
		t.Run(mode, func(t *testing.T) {
			old, source, block := uuid.NewString(), uuid.NewString(), uuid.NewString()
			instruction := "把刚才的地点生成地图。"
			historical := TableSource{SourceID: strings.ToUpper(old), Anchor: TextAnchor{Quote: "早上去河边。"}}
			s := Snapshot{Revision: 7, Transcript: "不应传给模型的完整历史录音", KnownSourceIDs: []string{old, source}, PendingUtterances: []SourceUtterance{{ID: source, Text: instruction}}, JourneySourceContext: []TableSource{historical}}
			c := JourneyCreation{ID: uuid.NewString(), BlockID: block, MapID: uuid.NewString(), SourceID: source, Instruction: instruction, Title: "河边散步", Visits: []JourneyVisit{{ID: uuid.NewString(), Expression: "河边", SourceID: historical.SourceID, Anchor: TextAnchor{Quote: "河边"}}}}
			r := Revision{BaseRevision: 7, TranscriptRevision: 9, JourneyCreations: []JourneyCreation{c}, ConsumedSourceIDs: []string{source}, SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: instruction, Role: "instruction", BlockIDs: []string{block}}}}}}
			encoded, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err = json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"diagramCreations", "journeyEdits", "timelineEdits", "timelineCreations", "tableCreations", "tableEdits", "tableResolutions", "formatCommands", "moveCommands", "paragraphCommands", "paragraphResolutions", "moveResolutions", "formatResolutions"} {
				fields[key] = []any{}
			}
			switch mode {
			case "missing":
				delete(fields, "journeyCreations")
			case "null":
				fields["journeyCreations"] = nil
			case "unknown geographic field":
				fields["journeyCreations"].([]any)[0].(map[string]any)["latitude"] = 30.0
			}
			response, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			requests := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests <- struct{}{}
				var body struct {
					Input string `json:"input"`
					Store bool   `json:"store"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if req.URL.Path != "/responses" || req.Method != http.MethodPost || body.Store {
					t.Error("unexpected model request")
				}
				if strings.Contains(body.Input, s.Transcript) {
					t.Error("full historical transcript leaked")
				}
				var input rewriteModelDocument
				if err := json.Unmarshal([]byte(body.Input), &input); err != nil {
					t.Error(err)
				}
				if !reflect.DeepEqual(input.JourneySourceContext, s.JourneySourceContext) || input.BaseRevision != 7 || input.TranscriptRevision != 9 {
					t.Error("source or revision changed in projection")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 20, "output_tokens": 30}, "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(response)}}}}})
			}))
			defer server.Close()
			result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "fixture", Model: "fixture", HTTP: server.Client()}).Rewrite(context.Background(), s, 9)
			select {
			case <-requests:
			default:
				t.Fatal("model transport was not exercised", err)
			}
			if mode == "valid" {
				if err != nil || !reflect.DeepEqual(result.Revision.JourneyCreations, r.JourneyCreations) {
					t.Fatal("journey did not round trip", err)
				}
			} else if !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid response accepted", err)
			}
			if result.InputTokens != 20 || result.OutputTokens != 30 {
				t.Fatal("lost usage on response validation")
			}
		})
	}
}
