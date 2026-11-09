package voice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiagramEditModelResponseContract(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "null", "unknown field", "multiple actions", "content"} {
		t.Run(mode, func(t *testing.T) {
			r, snapshot := diagramEditRevisionFixture()
			r.TranscriptRevision = 3
			data, _ := json.Marshal(r)
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			for name, raw := range voiceRevisionSchema()["properties"].(map[string]any) {
				if field, ok := raw.(map[string]any); ok && field["type"] == "array" && fields[name] == nil {
					fields[name] = []any{}
				}
			}
			command := fields["diagramEdits"].([]any)[0].(map[string]any)
			switch mode {
			case "missing":
				delete(fields, "diagramEdits")
			case "null":
				fields["diagramEdits"] = nil
			case "unknown field":
				command["archiveID"] = "not model owned"
			case "multiple actions":
				command["actions"].([]any)[0].(map[string]any)["remove"] = map[string]any{"nodeID": snapshot.DiagramContext[0].Nodes[1].ID}
			case "content":
				fields["sourcePartitions"].([]any)[0].(map[string]any)["segments"].([]any)[0].(map[string]any)["role"] = "content"
			}
			response, _ := json.Marshal(fields)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				var body struct {
					Instructions string `json:"instructions"`
					Store        bool   `json:"store"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Store || !strings.Contains(body.Instructions, "diagramEdits") {
					t.Error("missing privacy or edit instruction")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 20, "output_tokens": 30}, "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(response)}}}}})
			}))
			defer server.Close()
			result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "fixture", Model: "fixture", HTTP: server.Client()}).Rewrite(context.Background(), snapshot, 3)
			if mode == "valid" {
				if err != nil || len(result.Revision.DiagramEdits) != 1 {
					t.Fatal("valid edit rejected", err)
				}
			} else if !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid edit accepted", err)
			}
			if result.InputTokens != 20 || result.OutputTokens != 30 {
				t.Fatal("metering lost")
			}
		})
	}
}
