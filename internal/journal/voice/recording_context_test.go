package voice

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func recordingContextFixture(t *testing.T) RecordingContext {
	t.Helper()
	data, err := os.ReadFile("testdata/recording_two_synthetic_voices.json")
	if err != nil {
		t.Fatal(err)
	}
	a, err := parseRecordingAnalysis(data, recordingTask, 22649)
	if err != nil {
		t.Fatal(err)
	}
	return RecordingContext{Mode: "narrative", NarratorSpeakerID: "1", Speakers: []RecordingSpeaker{{"1", "我"}, {"2", "同行者"}}, Analysis: a}
}
func TestRecordingContextRejectsInvalidIdentitiesAndIntervals(t *testing.T) {
	base := recordingContextFixture(t)
	if err := base.Validate(base.Analysis.Text); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*RecordingContext)
	}{
		{"mode", func(r *RecordingContext) { r.Mode = "instructions" }},
		{"narrator", func(r *RecordingContext) { r.NarratorSpeakerID = "missing" }},
		{"duplicate identity", func(r *RecordingContext) { r.Speakers = append(r.Speakers, r.Speakers[0]) }},
		{"duplicate utterance", func(r *RecordingContext) { r.Analysis.Utterances[1].ID = r.Analysis.Utterances[0].ID }},
		{"range", func(r *RecordingContext) { r.Analysis.Utterances[0].EndMilliseconds = 999999 }},
		{"version", func(r *RecordingContext) { r.Analysis.Version = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var r RecordingContext
			json.Unmarshal(raw, &r)
			tc.change(&r)
			if r.Validate(r.Analysis.Text) == nil {
				t.Fatal("accepted invalid recording context")
			}
		})
	}
	if base.Validate("different source") == nil {
		t.Fatal("accepted mismatched transcript")
	}
}
func TestRecordingSpeakerEvidenceDoesNotInventOrMergePeople(t *testing.T) {
	a := recordingContextFixture(t).Analysis
	a.Utterances = append(a.Utterances, RecordingUtterance{ID: "short-1", Speaker: "3", Text: "嗯。"}, RecordingUtterance{ID: "short-2", Speaker: "3", Text: "哈哈哈。"})
	before, _ := json.Marshal(a)
	e := a.SpeakerEvidence()
	after, _ := json.Marshal(a)
	if string(before) != string(after) {
		t.Fatal("evidence summary changed source")
	}
	if len(e) != 3 || !e[0].HasLexicalSpeech || !e[1].HasLexicalSpeech || e[2].HasLexicalSpeech {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(e[2].UtteranceIDs, []string{"short-1", "short-2"}) {
		t.Fatal("short evidence was lost")
	}
	if e[2].SpeakerID != "3" {
		t.Fatal("silently merged provider identity")
	}
}
func TestRecordingContextRemainsOptionalForLiveSnapshots(t *testing.T) {
	s := Snapshot{WritingStyle: StyleNatural, Blocks: []Block{}, Transcript: "普通口述"}
	data, _ := json.Marshal(s)
	var wire map[string]any
	json.Unmarshal(data, &wire)
	if _, ok := wire["recordingContext"]; ok {
		t.Fatal("ordinary live context changed")
	}
}

func TestRecordingDialoguePreservesEverySourceTurnAndRenamesWithoutASR(t *testing.T) {
	r := recordingContextFixture(t)
	r.Mode = "dialogue"
	original, _ := json.Marshal(r.Analysis)
	turns, err := RenderRecordingDialogue(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 4 {
		t.Fatal(turns)
	}
	for i, turn := range turns {
		u := r.Analysis.Utterances[i]
		if turn.Text != u.Text || turn.StartMilliseconds != u.StartMilliseconds || turn.EndMilliseconds != u.EndMilliseconds || !reflect.DeepEqual(turn.UtteranceIDs, []string{u.ID}) {
			t.Fatal("dialogue changed source", i)
		}
	}
	r.Speakers[1].Name = "朋友"
	renamed, err := RenderRecordingDialogue(r)
	if err != nil || renamed[1].SpeakerName != "朋友" || renamed[3].SpeakerName != "朋友" {
		t.Fatal(renamed, err)
	}
	after, _ := json.Marshal(r.Analysis)
	if string(after) != string(original) {
		t.Fatal("rename rewrote analysis")
	}
	r.Speakers = r.Speakers[:1]
	unknown, _ := RenderRecordingDialogue(r)
	if unknown[1].SpeakerName != "未确认声音" || unknown[1].SpeakerID != "2" {
		t.Fatal("invented unknown identity")
	}
}
func TestRecordingEditorialContextOmitsProviderBookkeeping(t *testing.T) {
	r := recordingContextFixture(t)
	s := Snapshot{Transcript: r.Analysis.Text, RecordingContext: &r}
	data, _ := json.Marshal(editorialDocument(s))
	var wire map[string]any
	json.Unmarshal(data, &wire)
	c := wire["recordingContext"].(map[string]any)
	if _, ok := c["analysis"]; ok {
		t.Fatal("duplicate full ASR payload in model prompt")
	}
	if len(c["utterances"].([]any)) != len(r.Analysis.Utterances) {
		t.Fatal("lost speaker source")
	}
}

func TestRecordingReviewFlagsLostNumericQualifierWithoutChangingDraft(t *testing.T) {
	a := RecordingAnalysis{Utterances: []RecordingUtterance{{ID: "fare", Text: "票价好像128元。"}}}
	if issues := ReviewRecordingDraft(a, "票价128元。"); len(issues) != 1 || issues[0].UtteranceID != "fare" {
		t.Fatal(issues)
	}
	if issues := ReviewRecordingDraft(a, "票价大概128元。"); len(issues) != 0 {
		t.Fatal(issues)
	}
	if issues := ReviewRecordingDraft(a, "这个编号是9128。"); len(issues) != 0 {
		t.Fatal("matched a substring of another number")
	}
}

func TestDialoguePreviewCannotDropRewriteOrDuplicateTurns(t *testing.T) {
	r := recordingContextFixture(t)
	r.Mode = "dialogue"
	id := "5b7b2fe7-a8a2-48e3-ad3f-620e86fd9981"
	s := Snapshot{Transcript: r.Analysis.Text, RecordingContext: &r, Blocks: []Block{{ID: id, Text: "原正文"}}}
	canonical, err := RecordingDialogueText(r)
	if err != nil {
		t.Fatal(err)
	}
	revision := Revision{Patches: []Patch{{ID: id, Text: canonical}}}
	if err := ValidateRecordingDialogueRevision(s, revision); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"遗漏所有发言", canonical + "\n\n" + canonical} {
		revision.Patches[0].Text = text
		if ValidateRecordingDialogueRevision(s, revision) == nil {
			t.Fatal("accepted missing or duplicate dialogue")
		}
	}
}

func TestRecordingReviewQualifierDoesNotLockEveryNumberInSentence(t *testing.T) {
	a := RecordingAnalysis{Utterances: []RecordingUtterance{{ID: "schedule", Text: "已经开了12公里了，再开下去就可能没油。"}}}
	if issues := ReviewRecordingDraft(a, "已经开了12公里。"); len(issues) != 0 {
		t.Fatal("unrelated uncertainty attached to known number", issues)
	}
	a.Utterances[0].Text = "票价128元好像。"
	if issues := ReviewRecordingDraft(a, "票价128元，可能明天出发。"); len(issues) != 1 {
		t.Fatal("unrelated maybe hid the lost price qualifier", issues)
	}
}
