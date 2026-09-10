package voice

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDialogueModelPlacesReferenceAndServerPreservesExactTurns(t *testing.T) {
	block := uuid.NewString()
	analysis := RecordingAnalysis{Version: RecordingAnalysisVersion, TaskID: uuid.NewString(), Milliseconds: 2000, Text: "我很害怕。我陪着你。", Utterances: []RecordingUtterance{
		{ID: uuid.NewString(), Speaker: "wife", StartMilliseconds: 0, EndMilliseconds: 900, Text: "我很害怕。"},
		{ID: uuid.NewString(), Speaker: "me", StartMilliseconds: 1000, EndMilliseconds: 1900, Text: "我陪着你。"},
	}}
	s := Snapshot{Revision: 1, Blocks: []Block{{ID: block, Text: "旧独白"}}, Transcript: analysis.Text,
		RecordingContext: &RecordingContext{Mode: "dialogue", NarratorSpeakerID: "me", Speakers: []RecordingSpeaker{{ID: "wife", Name: "妻子"}, {ID: "me", Name: "我"}}, Analysis: analysis}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Input string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var input struct {
			Document struct {
				RecordingContext struct {
					DialogueText string `json:"dialogueText"`
				} `json:"recordingContext"`
			} `json:"document"`
		}
		if err := json.Unmarshal([]byte(request.Input), &input); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		marker := input.Document.RecordingContext.DialogueText
		if !strings.HasPrefix(marker, "[journal-dialogue:") {
			t.Error("missing typed insertion reference")
		}
		revision := Revision{BaseRevision: 1, TranscriptRevision: 2, Patches: []Patch{{ID: block, Text: marker}}, Questions: []string{}}
		encoded, _ := json.Marshal(revision)
		content := []map[string]string{{"type": "output_text", "text": string(encoded)}}
		output := []map[string]any{{"content": content}}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 11, "output_tokens": 22}, "output": output})
	}))
	defer server.Close()
	got, err := (ArkRewriter{BaseURL: server.URL, APIKey: "test", Model: "test", HTTP: server.Client()}).Rewrite(context.Background(), s, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision.Patches[0].Text != "妻子：我很害怕。\n\n我：我陪着你。" {
		t.Fatal("dialogue was not expanded exactly")
	}
	if got.InputTokens != 11 || got.OutputTokens != 22 {
		t.Fatal("lost metering")
	}
}
