package voice

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamTraceDistinguishesProviderOmissionFromParserLossWithoutText(t *testing.T) {
	end, outside := 1000, 16000
	raw := []providerStreamUtterance{
		{Text: "private words", Definite: true, Start: json.RawMessage(`0`), End: &end},
		{Text: "sensitive details", Definite: true, Start: json.RawMessage(`0`), End: &outside},
		{Text: "missing range"},
	}
	parsed := Transcript{Text: "earlier sentence private words", Utterances: streamUtterances(raw)}
	trace := makeStreamTrace(raw, parsed)
	trace.merged(parsed)
	if trace.RawCount != 3 || trace.ParsedCount != 1 || trace.OutsideTimes != 1 || trace.MissingTimes != 1 || trace.Relation != "suffix" {
		t.Fatal(trace)
	}
	data, err := json.Marshal(trace)
	if err != nil || strings.Contains(string(data), "private") || strings.Contains(string(data), "sensitive") || strings.Contains(string(data), "earlier") {
		t.Fatal("content leaked", err)
	}
}
