package voice

import (
	"strings"
	"unicode/utf8"
)

// StreamTrace contains structure only. It deliberately has no transcript,
// speaker identifiers, audio, credentials or arbitrary provider strings.
// One bounded list is emitted when a development ASR connection finishes.
type StreamTrace struct {
	Final            bool   `json:"final"`
	TextCharacters   int    `json:"textCharacters"`
	RawCount         int    `json:"rawCount"`
	RawDefinite      int    `json:"rawDefinite"`
	MissingTimes     int    `json:"missingTimes"`
	OutsideTimes     int    `json:"outsideTimes"`
	RawFirstStart    int    `json:"rawFirstStart"`
	RawLastEnd       int    `json:"rawLastEnd"`
	ParsedCount      int    `json:"parsedCount"`
	MergedCount      int    `json:"mergedCount"`
	MergedFirstStart int    `json:"mergedFirstStart"`
	Relation         string `json:"relation"`
}

func makeStreamTrace(raw []providerStreamUtterance, parsed Transcript) StreamTrace {
	v := StreamTrace{Final: parsed.Final, TextCharacters: utf8.RuneCountInString(parsed.Text), RawCount: len(raw), ParsedCount: len(parsed.Utterances), RawFirstStart: -1, RawLastEnd: -1}
	for i, u := range raw {
		if u.Definite {
			v.RawDefinite++
		}
		if len(u.Start) == 0 || u.End == nil {
			v.MissingTimes++
		}
		start, _, valid := streamUtteranceStart(u.Start, i)
		if !valid || u.End == nil {
			continue
		}
		if start < 0 || *u.End < start || *u.End > 15100 {
			v.OutsideTimes++
		}
		if i == 0 {
			if len(u.Start) > 0 {
				v.RawFirstStart = start
			}
		}
		v.RawLastEnd = *u.End
	}
	return v
}
func (v *StreamTrace) merged(t Transcript) {
	v.MergedCount = len(t.Utterances)
	v.MergedFirstStart = -1
	if len(t.Utterances) > 0 {
		v.MergedFirstStart = t.Utterances[0].StartMilliseconds
	}
	var text strings.Builder
	for _, u := range t.Utterances {
		text.WriteString(u.Text)
	}
	switch {
	case t.Text == text.String():
		v.Relation = "exact"
	case text.Len() == 0:
		v.Relation = "missing"
	case strings.HasSuffix(t.Text, text.String()):
		v.Relation = "suffix"
	default:
		v.Relation = "different"
	}
}
