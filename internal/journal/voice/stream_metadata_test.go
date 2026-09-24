package voice

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamMetadataTopLevelAndNestedEvidence(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		volume       float64
	}{
		{"direct", `"speaker":"2","emotion":"happy","volume":0,"speech_rate":3.25,"additions":{"volume":"7.5"}`, 0},
		{"nested", `"additions":{"speaker_id":"2","emotion":"happy","volume":"7.5","speech_rate":"3.25"}`, 7.5},
		{"fallback", `"speaker":{},"volume":null,"additions":{"speaker":"2","acoustic_emotion":"happy","volume":7.5,"speech_rate":3.25}`, 7.5},
	} {
		t.Run(test.name, func(t *testing.T) {
			var raw []providerStreamUtterance
			if err := json.Unmarshal([]byte(`[{"text":"散步","start_time":0,"end_time":1000,`+test.fields+`}]`), &raw); err != nil {
				t.Fatal(err)
			}
			got := streamUtterances(raw)
			if len(got) != 1 {
				t.Fatal("lost utterance")
			}
			u := got[0]
			if u.Speaker != "2" || u.AcousticEmotion != "happy" || u.Volume == nil || *u.Volume != test.volume || u.SpeechRate == nil || *u.SpeechRate != 3.25 {
				t.Fatalf("incorrect evidence: %+v", u)
			}
		})
	}
}

func TestStreamMetadataDoesNotFabricateOrAcceptUnboundedValues(t *testing.T) {
	for _, fields := range []string{``, `,"volume":"NaN","speech_rate":"+Inf","speaker":[]`,
		`,"speaker":"` + strings.Repeat("人", 129) + `","emotion":"` + strings.Repeat("喜", 513) + `"`} {
		var raw []providerStreamUtterance
		if err := json.Unmarshal([]byte(`[{"text":"散步","start_time":0,"end_time":1000`+fields+`}]`), &raw); err != nil {
			t.Fatal(err)
		}
		u := streamUtterances(raw)[0]
		if u.Speaker != "" || u.AcousticEmotion != "" || u.Volume != nil || u.SpeechRate != nil {
			t.Fatal("fabricated metadata")
		}
	}
}
