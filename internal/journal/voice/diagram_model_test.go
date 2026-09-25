package voice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestDiagramModelResponseContract(t *testing.T) {
	if !slices.Contains(voiceRevisionSchema()["required"].([]string), "diagramCreations") {
		t.Fatal("missing required field")
	}
	for _, mode := range []string{"valid", "empty", "missing", "null", "missing edits", "null edits", "local archive", "instruction evidence"} {
		t.Run(mode, func(t *testing.T) {
			r, s := diagramRevisionFixture()
			r.TranscriptRevision = 3
			if mode == "empty" {
				r.DiagramCreations = []DiagramCreation{}
				r.ConsumedSourceIDs = []string{}
				r.SourcePartitions = []SourcePartition{}
				s = Snapshot{}
			}
			encoded, _ := json.Marshal(r)
			fields := map[string]any{}
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"diagramEdits", "journeyEdits", "journeyCreations", "timelineEdits", "timelineCreations", "tableCreations", "tableEdits", "tableResolutions", "formatCommands", "moveCommands", "paragraphCommands", "paragraphResolutions", "moveResolutions", "formatResolutions"} {
				fields[key] = []any{}
			}
			switch mode {
			case "missing edits":
				delete(fields, "diagramEdits")
			case "null edits":
				fields["diagramEdits"] = nil
			case "missing":
				delete(fields, "diagramCreations")
			case "null":
				fields["diagramCreations"] = nil
			case "local archive", "instruction evidence":
				graph := fields["diagramCreations"].([]any)[0].(map[string]any)["diagram"].(map[string]any)
				source := graph["nodes"].([]any)[0].(map[string]any)["sources"].([]any)[0].(map[string]any)
				if mode == "local archive" {
					source["archiveID"] = "forbidden"
				} else {
					source["anchor"].(map[string]any)["quote"] = r.DiagramCreations[0].Instruction
				}
			}
			response, _ := json.Marshal(fields)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					Instructions string `json:"instructions"`
					Store        bool   `json:"store"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Store || !strings.Contains(body.Instructions, "diagramCreations") {
					t.Error("missing privacy or diagram instructions")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 20, "output_tokens": 30}, "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(response)}}}}})
			}))
			defer server.Close()
			result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "fixture", Model: "fixture", HTTP: server.Client()}).Rewrite(context.Background(), s, 3)
			if mode == "valid" || mode == "empty" {
				if err != nil || result.Revision.DiagramCreations == nil || len(result.Revision.DiagramCreations) != len(r.DiagramCreations) {
					t.Fatal("valid graph response rejected", err)
				}
			} else if !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid response accepted", err)
			}
			if result.InputTokens != 20 || result.OutputTokens != 30 {
				t.Fatal("metering lost during validation")
			}
		})
	}
}
