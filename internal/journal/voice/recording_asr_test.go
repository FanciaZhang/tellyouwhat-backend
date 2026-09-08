package voice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"
)

const recordingTask = "d23db388-c34a-4eab-8a98-54572a69020e"

func TestRecordingRealProviderFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/recording_two_synthetic_voices.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseRecordingAnalysis(data, recordingTask, 22649)
	if err != nil {
		t.Fatal(err)
	}
	speakers := []string{}
	for _, u := range r.Utterances {
		speakers = append(speakers, u.Speaker)
		if u.AcousticEmotion != "neutral" {
			t.Fatal(u)
		}
	}
	if !reflect.DeepEqual(speakers, []string{"1", "2", "1", "2"}) {
		t.Fatal(speakers)
	}
	again, _ := parseRecordingAnalysis(data, recordingTask, 22649)
	if !reflect.DeepEqual(r, again) {
		t.Fatal("unstable utterance identity")
	}
	other, _ := parseRecordingAnalysis(data, "1b3297e4-e4d4-4e6f-9499-51b0ac7ccaa2", 22649)
	if r.Utterances[0].ID == other.Utterances[0].ID {
		t.Fatal("cross task identity collision")
	}
}
func TestRecordingRejectsUntrustedRangesAndPreservesRawEmotion(t *testing.T) {
	data, _ := os.ReadFile("testdata/recording_two_synthetic_voices.json")
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"negative", func(u map[string]any) { u["start_time"] = -1 }},
		{"outside audio", func(u map[string]any) { u["end_time"] = 999999 }},
		{"reversed", func(u map[string]any) { u["start_time"] = 6000 }},
		{"speaker object", func(u map[string]any) { u["additions"].(map[string]any)["speaker"] = map[string]string{"bad": "value"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire map[string]any
			json.Unmarshal(data, &wire)
			tc.mutate(wire["result"].(map[string]any)["utterances"].([]any)[0].(map[string]any))
			bad, _ := json.Marshal(wire)
			if _, err := parseRecordingAnalysis(bad, recordingTask, 22649); err == nil {
				t.Fatal("accepted invalid provider data")
			}
		})
	}
	var wire map[string]any
	json.Unmarshal(data, &wire)
	additions := wire["result"].(map[string]any)["utterances"].([]any)[0].(map[string]any)["additions"].(map[string]any)
	additions["emotion"] = "  a future provider phrase  "
	delete(additions, "speaker")
	changed, _ := json.Marshal(wire)
	r, err := parseRecordingAnalysis(changed, recordingTask, 22649)
	if err != nil || r.Utterances[0].AcousticEmotion != "  a future provider phrase  " || r.Utterances[0].Speaker != "" {
		t.Fatal(r, err)
	}
}
func TestRecordingProtocolUsesStandardAndDoesNotFollowRedirect(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Api-Request-Id") != recordingTask || r.Header.Get("X-Api-Key") != "test-key" {
			t.Error("incorrect auth or task identity")
		}
		w.Header().Set("X-Api-Status-Code", "20000002")
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	a := RecordingASR{Config: ASRConfig{URL: server.URL, ResourceID: "volc.seedasr.auc", APIKey: "test-key"}, Client: &http.Client{Timeout: time.Second}}
	if _, err := a.Query(context.Background(), recordingTask, 22649); !errors.Is(err, ErrRecordingPending) {
		t.Fatal(err)
	}
	a.Config.ResourceID = "volc.bigasr.auc_turbo"
	if _, err := a.Query(context.Background(), recordingTask, 22649); err == nil || calls != 1 {
		t.Fatal("turbo accepted")
	}
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	a.Config.URL = redirect.URL
	a.Config.ResourceID = "volc.seedasr.auc"
	if _, err := a.Query(context.Background(), recordingTask, 22649); err == nil || targetCalls != 0 {
		t.Fatal("forwarded credentials")
	}
}
