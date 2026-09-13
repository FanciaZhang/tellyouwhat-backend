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

func TestStreamOmittedFirstStartRetainsOriginalTurnAndProvenance(t *testing.T) {
	// Reproduces the actual provider shape: start_time is absent on the first
	// utterance, but its end, words, speaker and emotion are present.
	raw := []byte(`{"result":{"text":"第一句。第二句。","utterances":[{"text":"第一句。","end_time":4022,"definite":true,"additions":{"speaker_id":"2","emotion":"provider-original"},"words":[{"text":"第一句","end_time":3800}]},{"text":"第二句。","start_time":4040,"end_time":6162,"definite":true,"additions":{"speaker_id":"1"}}]}}`)
	packet := asrPacket(9, true, raw)
	packet[2] = 0x10
	result, err := parseASR(packet)
	if err != nil || len(result.Utterances) != 2 {
		t.Fatal(result, err)
	}
	first := result.Utterances[0]
	if first.Text != "第一句。" || first.StartMilliseconds != 0 || !first.ProviderStartUnavailable || first.EndMilliseconds != 4022 || first.Speaker != "2" || first.AcousticEmotion != "provider-original" || len(first.Words) != 1 {
		t.Fatal(first)
	}
	if result.Utterances[1].ProviderStartUnavailable {
		t.Fatal("marked a returned onset as absent")
	}
	bounded := boundedStreamUtterances(result.Utterances, 15000)
	if len(bounded) != 2 || bounded[0].Text+bounded[1].Text != result.Text || !bounded[0].ProviderStartUnavailable {
		t.Fatal(bounded)
	}
}

func TestStreamMissingStartDoesNotAcceptNullOrInventLaterTurnRanges(t *testing.T) {
	for _, raw := range []string{
		`[{"text":"bad","start_time":null,"end_time":1000}]`,
		`[{"text":"bad","start_time":"0","end_time":1000}]`,
		`[{"text":"bad","start_time":-1,"end_time":1000}]`,
		`[{"text":"bad"}]`,
	} {
		var source []providerStreamUtterance
		if err := json.Unmarshal([]byte(raw), &source); err != nil {
			t.Fatal(err)
		}
		if got := streamUtterances(source); len(got) != 0 {
			t.Fatal("accepted malformed onset", got)
		}
	}
	var source []providerStreamUtterance
	if err := json.Unmarshal([]byte(`[{"text":"first","start_time":0,"end_time":1000},{"text":"unknown onset","end_time":2000}]`), &source); err != nil {
		t.Fatal(err)
	}
	if got := streamUtterances(source); len(got) != 1 || got[0].Text != "first" {
		t.Fatal("assigned a later turn the recording origin", got)
	}
}
