package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
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

// This fixture records a failed product-accuracy gate. Parsing success does not
// mean correct diarization. Six input voices were collapsed into three clusters.
func TestRecordingSixVoiceFailureRemainsObservable(t *testing.T) {
	data, err := os.ReadFile("testdata/recording_six_synthetic_voices.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseRecordingAnalysis(data, recordingTask, 56392)
	if err != nil {
		t.Fatal(err)
	}
	speakers := map[string]bool{}
	for _, u := range r.Utterances {
		speakers[u.Speaker] = true
	}
	if len(speakers) != 3 || len(r.Utterances) != 9 {
		t.Fatal("fixture changed; reassess the documented failed acceptance gate")
	}
	// Do not silently create six synthetic identities from expectations.
	if r.Utterances[3].Text != "我觉得坐火车更轻松开车可能会很累我想带上相机，给大家拍一张合影。" {
		t.Fatal("merged source utterance must remain auditable")
	}
}

func TestRecordingRejectsMalformedAudioBeforeBilling(t *testing.T) {
	a := RecordingASR{Config: ASRConfig{ResourceID: "volc.seedasr.auc"}}
	for _, audio := range [][]byte{nil, make([]byte, 44), make([]byte, 100)} {
		if err := a.Submit(context.Background(), recordingTask, audio); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}

func TestRecordingSubmissionStreamsCanonicalAudioAndFlags(t *testing.T) {
	pcm := bytes.Repeat([]byte{17, 23}, 16000)
	wav := make([]byte, 44+len(pcm))
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(pcm)))
	copy(wav[44:], pcm)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if int64(len(raw)) != r.ContentLength {
			t.Error("incorrect streaming content length")
		}
		var wire struct {
			Audio struct {
				Data   []byte
				Format string
			}
			Request struct {
				Speaker    bool `json:"enable_speaker_info"`
				Emotion    bool `json:"enable_emotion_detection"`
				Utterances bool `json:"show_utterances"`
			}
		}
		if json.Unmarshal(raw, &wire) != nil || !bytes.Equal(wire.Audio.Data, wav) || wire.Audio.Format != "wav" || !wire.Request.Speaker || !wire.Request.Emotion || !wire.Request.Utterances {
			t.Error("stream altered provider payload")
		}
		w.Header().Set("X-Api-Status-Code", "20000000")
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	a := RecordingASR{Config: ASRConfig{URL: server.URL, ResourceID: "volc.seedasr.auc"}}
	if err := a.Submit(context.Background(), recordingTask, wav); err != nil {
		t.Fatal(err)
	}
}
