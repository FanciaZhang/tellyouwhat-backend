package voice

import "encoding/json"

// Development diagnostics expose only known-field types and audio offsets.
// Never emit transcript, arbitrary metadata, user identifiers or audio bytes.
type StreamSchema struct {
	Start             int    `json:"start"`
	StartType         string `json:"startType"`
	EndType           string `json:"endType"`
	End               int    `json:"end"`
	Speaker           string `json:"speakerType"`
	SpeakerID         string `json:"speakerIDType"`
	AdditionSpeaker   string `json:"additionSpeakerType"`
	AdditionSpeakerID string `json:"additionSpeakerIDType"`
	Volume            string `json:"volumeType"`
	SpeechRate        string `json:"speechRateType"`
}

func streamSchema(raw []byte) []StreamSchema {
	var wire struct {
		Result struct {
			Utterances []map[string]json.RawMessage `json:"utterances"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &wire) != nil || len(wire.Result.Utterances) > 256 {
		return nil
	}
	var result []StreamSchema
	for _, u := range wire.Result.Utterances {
		var a map[string]json.RawMessage
		_ = json.Unmarshal(u["additions"], &a)
		v := StreamSchema{StartType: jsonKind(u["start_time"]), EndType: jsonKind(u["end_time"]), Speaker: jsonKind(u["speaker"]), SpeakerID: jsonKind(u["speaker_id"]), AdditionSpeaker: jsonKind(a["speaker"]), AdditionSpeakerID: jsonKind(a["speaker_id"]), Volume: jsonKind(a["volume"]), SpeechRate: jsonKind(a["speech_rate"])}
		_ = json.Unmarshal(u["start_time"], &v.Start)
		_ = json.Unmarshal(u["end_time"], &v.End)
		result = append(result, v)
	}
	return result
}
func jsonKind(raw []byte) string {
	if len(raw) == 0 {
		return "missing"
	}
	switch raw[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 'n':
		return "null"
	case 't', 'f':
		return "boolean"
	default:
		return "number"
	}
}
