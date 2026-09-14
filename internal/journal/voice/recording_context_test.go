package voice

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
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

func sourcePassage(blockID string, paragraphIndex int, sourceIDs ...string) PassageSource {
	return PassageSource{BlockID: blockID, ParagraphIndex: paragraphIndex, SourceIDs: sourceIDs}
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

func TestStreamingFinalContextCanFinishBodyAndPlaceEmotionsWithoutInventingPeople(t *testing.T) {
	r := recordingContextFixture(t)
	r.Mode = "stream"
	r.NarratorSpeakerID = ""
	r.Speakers = nil
	r.Analysis.Utterances[0].AcousticEmotion = "happy"
	block := "5b7b2fe7-a8a2-48e3-ad3f-620e86fd9981"
	s := Snapshot{Revision: 4, Transcript: r.Analysis.Text, RecordingContext: &r,
		Blocks: []Block{{ID: block, Text: "今天的手记已经整理好了。"}}}
	valid := Revision{BaseRevision: 4, TranscriptRevision: 7,
		Passages:       []PassageSource{sourcePassage(block, 0, r.Analysis.Utterances[0].ID)},
		OverallEmotion: "happy", Emotions: []EmotionPlacement{{
			BlockID: block, AnchorText: "手记已经整理好了", SourceID: r.Analysis.Utterances[0].ID, Kind: "happy",
		}}}
	if err := valid.Validate(s); err != nil {
		t.Fatal(err)
	}
	withPatch := valid
	withPatch.Patches = []Patch{{ID: block, Text: "今天的手记已经整理好了，补上末尾。"}}
	if err := withPatch.Validate(s); err != nil {
		t.Fatal("stream finalization could not incorporate the last transcript", err)
	}
	prepared, err := PrepareRewrite(context.Background(), s, 7, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Instructions string `json:"instructions"`
	}
	if json.Unmarshal(prepared.Body, &body) != nil ||
		!strings.Contains(body.Instructions, "录音关闭且所有转写已收齐") ||
		!strings.Contains(body.Instructions, "不得从声音编号") {
		t.Fatal("stream-final instruction was not selected")
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
	for _, issue := range ReviewRecordingDraft(a, "这个编号是9128。") {
		if issue.Code == "numeric_uncertainty_needs_review" {
			t.Fatal("matched a substring of another number")
		}
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
	passages := make([]PassageSource, len(r.Analysis.Utterances))
	for index, utterance := range r.Analysis.Utterances {
		passages[index] = sourcePassage(id, index, utterance.ID)
	}
	revision := Revision{Patches: []Patch{{ID: id, Text: canonical}}, Passages: passages, OverallEmotion: "calm"}
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

func TestRecordingEmotionRevisionRequiresTypedEvidenceAndUniqueAnchor(t *testing.T) {
	r := recordingContextFixture(t)
	r.Analysis.Utterances[0].AcousticEmotion = "happy"
	block := "5b7b2fe7-a8a2-48e3-ad3f-620e86fd9981"
	s := Snapshot{Revision: 3, Transcript: r.Analysis.Text, RecordingContext: &r,
		Blocks: []Block{{ID: block, Text: "这段山路很漂亮。后来下山了。"}}}
	source := r.Analysis.Utterances[0].ID
	valid := Revision{BaseRevision: 3, Passages: []PassageSource{sourcePassage(block, 0, source)}, OverallEmotion: "happy", Emotions: []EmotionPlacement{{
		BlockID: block, AnchorText: "这段山路很漂亮", SourceID: source, Kind: "happy",
	}}}
	if err := valid.Validate(s); err != nil {
		t.Fatal(err)
	}
	missingPlacement := valid
	missingPlacement.Emotions = nil
	if missingPlacement.Validate(s) == nil {
		t.Fatal("accepted an empty inline result despite non-neutral acoustic evidence")
	}
	neutral := s
	neutral.RecordingContext.Analysis.Utterances[0].AcousticEmotion = "neutral"
	if err := missingPlacement.Validate(neutral); err != nil {
		t.Fatal("required an inline marker for neutral-only acoustic evidence", err)
	}
	for name, mutate := range map[string]func(*Revision){
		"unknown kind":     func(value *Revision) { value.Emotions[0].Kind = "provider-happy" },
		"unknown source":   func(value *Revision) { value.Emotions[0].SourceID = "missing" },
		"ambiguous anchor": func(value *Revision) { value.Emotions[0].AnchorText = "山" },
		"missing overall":  func(value *Revision) { value.OverallEmotion = "" },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			value.Emotions = append([]EmotionPlacement(nil), valid.Emotions...)
			mutate(&value)
			if value.Validate(s) == nil {
				t.Fatal("accepted invalid emotion revision")
			}
		})
	}
	live := s
	live.RecordingContext = nil
	if valid.Validate(live) == nil {
		t.Fatal("accepted recording emotions on an incremental rewrite")
	}
}

func TestRecordingPassagesRequireContinuousUniqueSourcesAndRealParagraphs(t *testing.T) {
	r := recordingContextFixture(t)
	for index := range r.Analysis.Utterances {
		r.Analysis.Utterances[index].AcousticEmotion = "neutral"
	}
	block := "5b7b2fe7-a8a2-48e3-ad3f-620e86fd9981"
	s := Snapshot{Revision: 5, Transcript: r.Analysis.Text, RecordingContext: &r,
		Blocks: []Block{{ID: block, Text: "第一段整理。\n第二段整理。"}}}
	validRevision := func() Revision {
		return Revision{BaseRevision: 5, OverallEmotion: "calm", Passages: []PassageSource{
			sourcePassage(block, 0, r.Analysis.Utterances[0].ID, r.Analysis.Utterances[1].ID),
			sourcePassage(block, 1, r.Analysis.Utterances[2].ID),
		}}
	}
	if err := validRevision().Validate(s); err != nil {
		t.Fatal(err)
	}
	sharedBoundary := validRevision()
	sharedBoundary.Passages[1].SourceIDs = []string{
		r.Analysis.Utterances[1].ID,
		r.Analysis.Utterances[2].ID,
	}
	if err := sharedBoundary.Validate(s); err != nil {
		t.Fatal("one long source turn may legitimately support adjacent paragraphs", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Revision)
	}{
		{"missing passages", func(value *Revision) { value.Passages = nil }},
		{"noncontinuous sources", func(value *Revision) {
			value.Passages[0].SourceIDs = []string{r.Analysis.Utterances[0].ID, r.Analysis.Utterances[2].ID}
		}},
		{"unknown source", func(value *Revision) { value.Passages[1].SourceIDs = []string{"forged"} }},
		{"duplicate target", func(value *Revision) { value.Passages[1].ParagraphIndex = 0 }},
		{"missing paragraph", func(value *Revision) { value.Passages[1].ParagraphIndex = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := validRevision()
			tc.mutate(&value)
			if value.Validate(s) == nil {
				t.Fatal("accepted invalid passage provenance")
			}
		})
	}

	blank := s
	blank.Blocks = []Block{{ID: block, Text: " \n\t"}}
	if validRevision().Validate(blank) == nil {
		t.Fatal("accepted provenance for an empty body paragraph")
	}
	locked := s
	locked.MediaOnlyBlockIDs = []string{block}
	if validRevision().Validate(locked) == nil {
		t.Fatal("attached voice provenance to a media-only block")
	}
}

func TestRecordingEmotionSourceMustBelongToItsAnchorParagraph(t *testing.T) {
	r := recordingContextFixture(t)
	for index := range r.Analysis.Utterances {
		r.Analysis.Utterances[index].AcousticEmotion = "neutral"
	}
	r.Analysis.Utterances[0].AcousticEmotion = "happy"
	r.Analysis.Utterances[1].AcousticEmotion = "nervous"
	block := "5b7b2fe7-a8a2-48e3-ad3f-620e86fd9981"
	s := Snapshot{Revision: 2, Transcript: r.Analysis.Text, RecordingContext: &r,
		Blocks: []Block{{ID: block, Text: "第一段很开心。\n第二段有点紧张。"}}}
	revision := Revision{BaseRevision: 2, OverallEmotion: "happy", Passages: []PassageSource{
		sourcePassage(block, 0, r.Analysis.Utterances[0].ID),
		sourcePassage(block, 1, r.Analysis.Utterances[1].ID),
	}, Emotions: []EmotionPlacement{{
		BlockID: block, AnchorText: "有点紧张", SourceID: r.Analysis.Utterances[1].ID, Kind: "nervous",
	}}}
	if err := revision.Validate(s); err != nil {
		t.Fatal(err)
	}
	revision.Emotions[0].SourceID = r.Analysis.Utterances[0].ID
	if revision.Validate(s) == nil {
		t.Fatal("accepted emotion evidence linked to a different paragraph")
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
