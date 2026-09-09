package voice

import (
	"context"
	"encoding/json"
	"golang.org/x/net/websocket"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamMetadataPreservesSourceAndRejectsOutOfRange(t *testing.T) {
	raw := []byte(`{"result":{"text":"我有点害怕。","utterances":[{"text":"我有点害怕。","start_time":100,"end_time":1200,"definite":true,"additions":{"speaker_id":"2","emotion":"happy","volume":"-14.5","speech_rate":"3.2"},"words":[{"text":"我","start_time":100,"end_time":200}]},{"text":"bad","start_time":-1,"end_time":20},{"text":"bad","start_time":0,"end_time":90000}]}}`)
	packet := asrPacket(9, true, raw)
	packet[2] = 0x10
	result, err := parseASR(packet)
	if err != nil || result.Text != "我有点害怕。" || len(result.Utterances) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	u := result.Utterances[0]
	if u.Speaker != "2" || u.AcousticEmotion != "happy" || u.Volume == nil || *u.Volume != -14.5 || len(u.Words) != 1 {
		t.Fatal(u)
	}
	if len(boundedStreamUtterances(result.Utterances, 1000)) != 0 {
		t.Fatal("metadata beyond received audio accepted")
	}
}
func TestStreamMissingMetadataDoesNotInventNeutralOrZero(t *testing.T) {
	var source []providerStreamUtterance
	if err := json.Unmarshal([]byte(`[{"text":"嗯","start_time":0,"end_time":200,"additions":{"emotion":{},"speaker":[],"volume":null}}]`), &source); err != nil {
		t.Fatal(err)
	}
	result := streamUtterances(source)
	if len(result) != 1 || result[0].Speaker != "" || result[0].AcousticEmotion != "" || result[0].Volume != nil || result[0].SpeechRate != nil {
		t.Fatal(result)
	}
}

func TestStreamInsightRequestUsesOptimizedTwoPassContract(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var packet []byte
		if websocket.Message.Receive(ws, &packet) != nil || len(packet) < 8 {
			return
		}
		var payload struct {
			Request map[string]any `json:"request"`
		}
		if json.Unmarshal(packet[8:], &payload) == nil {
			requests <- payload.Request
		}
	}))
	defer server.Close()
	client := ASR{Config: ASRConfig{URL: strings.Replace(server.URL, "http://", "ws://", 1), APIKey: "test", ResourceID: "volc.seedasr.sauc.duration", StreamInsights: true}}
	connection, err := client.Open(context.Background(), []string{"山间小屋"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	select {
	case request := <-requests:
		for _, key := range []string{"enable_nonstream", "enable_speaker_info", "enable_emotion_detection", "show_utterances", "show_volume", "show_speech_rate"} {
			if request[key] != true {
				t.Fatalf("%s missing", key)
			}
		}
		if request["ssd_version"] != "200" || request["enable_ddc"] != false || request["result_type"] != "full" {
			t.Fatal("incorrect streaming contract")
		}
	case <-time.After(time.Second):
		t.Fatal("no provider request")
	}
}

func TestStreamBoundaryPaddingPreservesUtteranceAndOriginalEnd(t *testing.T) {
	input := []StreamUtterance{{Text: "继续说", StartMilliseconds: 12452, EndMilliseconds: 15002, Words: []StreamWord{{Text: "说", StartMilliseconds: 14900, EndMilliseconds: 15002}}}}
	result := boundedStreamUtterances(input, 15000)
	if len(result) != 1 || result[0].EndMilliseconds != 15000 || result[0].ProviderEndMilliseconds == nil || *result[0].ProviderEndMilliseconds != 15002 || result[0].Words[0].EndMilliseconds != 15000 {
		t.Fatal(result)
	}
	if input[0].Words[0].EndMilliseconds != 15002 {
		t.Fatal("mutated original")
	}
}
