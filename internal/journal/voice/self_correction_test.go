package voice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestIncrementalSelfCorrectionPreservesExactSourceWithoutInventingMention(t *testing.T) {
	block, source := uuid.NewString(), uuid.NewString()
	s := Snapshot{Blocks: []Block{{ID: block, Style: "body"}}, WritingStyle: StyleNatural,
		ActiveBlockIDs: []string{block}, KnownSourceIDs: []string{source},
		PendingUtterances: []SourceUtterance{{ID: source, Text: "我和小明去公园，不是小明，是小林。"}}}
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "spoken self correction", true: "missing correction target"}[invalid], func(t *testing.T) {
			r := Revision{BlockEdits: []BlockEdit{{Kind: "replace", ID: block, Text: "我和小林去公园。", Style: "body"}},
				Passages: []Passage{{BlockID: block, SourceIDs: []string{source}}}, ConsumedSourceIDs: []string{source},
				SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{
					{Text: "我和", Role: "content", BlockIDs: []string{block}},
					{Text: "小明", Role: "context", BlockIDs: []string{}},
					{Text: "去公园，", Role: "content", BlockIDs: []string{block}},
					{Text: "不是小明，是", Role: "context", BlockIDs: []string{}},
					{Text: "小林。", Role: "content", BlockIDs: []string{block}},
				}}}}
			if invalid {
				r.SourcePartitions[0].Segments[3].Role = "correction"
			}
			data, _ := json.Marshal(r)
			var fields map[string]any
			_ = json.Unmarshal(data, &fields)
			for name, raw := range voiceRevisionSchema()["properties"].(map[string]any) {
				if f, ok := raw.(map[string]any); ok && f["type"] == "array" && fields[name] == nil {
					fields[name] = []any{}
				}
			}
			response, _ := json.Marshal(fields)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					Instructions string
					Store        bool
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Store || !strings.Contains(body.Instructions, "本轮原话内部的自我纠正") {
					t.Error("missing private self-correction contract")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 20, "output_tokens": 30}, "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(response)}}}}})
			}))
			defer server.Close()
			result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "fixture", Model: "fixture", HTTP: server.Client()}).Rewrite(context.Background(), s, 0)
			if invalid {
				if !errors.Is(err, ErrInvalid) {
					t.Fatal("invalid correction accepted", err)
				}
			} else if err != nil || result.Revision.BlockEdits[0].Text != "我和小林去公园。" || len(result.Revision.Corrections) != 0 {
				t.Fatal("spoken correction or source evidence lost", err)
			}
			if result.InputTokens != 20 || result.OutputTokens != 30 {
				t.Fatal("lost metering")
			}
		})
	}
}
