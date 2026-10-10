package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestSelectiveNarrativeKeepsMainlineAndConsumesGroundedBackgroundOnly(t *testing.T) {
	author := uuid.NewString()
	source := func(text, person string) PolishTarget {
		turn := SourceUtterance{ID: uuid.NewString(), Text: text, PersonID: person}
		return PolishTarget{ID: uuid.NewString(), Text: text, SourceText: text, SourceIDs: []string{turn.ID}, Turns: []SourceUtterance{turn}, Style: "body", CompleteSource: true}
	}
	main := source("我和老婆商量买洗碗机，预算三千元。", author)
	background := source("老张，把三楼办公室的灯关掉。", "")
	p := PolishRequest{Narrator: &Narrator{PersonID: author, Name: "我"}, Targets: []PolishTarget{main, background}}
	omission := PolishOmission{SourceID: background.Turns[0].ID, Text: background.Turns[0].Text, Reason: "backgroundConversation"}
	output := func(o []PolishOmission, paragraphs []PolishParagraph) string {
		b, _ := json.Marshal(map[string]any{"paragraphs": paragraphs, "questions": []string{}, "omissions": o})
		return string(b)
	}
	prose := []PolishParagraph{{Text: "我和老婆商量买洗碗机，预算三千元。", Style: "body", TargetIDs: []string{main.ID}}}
	if r, err := decodePolish(output([]PolishOmission{omission}, prose), p); err != nil || len(r.Omissions) != 1 || len(r.Targets) != 2 {
		t.Fatal("grounded omission must leave a consumable target and original evidence", r, err)
	}
	for _, bad := range []PolishOmission{
		{SourceID: uuid.NewString(), Text: omission.Text, Reason: omission.Reason},
		{SourceID: omission.SourceID, Text: "关灯。", Reason: omission.Reason},
		{SourceID: main.Turns[0].ID, Text: main.Turns[0].Text, Reason: omission.Reason},
		{SourceID: omission.SourceID, Text: omission.Text, Reason: "irrelevant"},
	} {
		if _, err := decodePolish(output([]PolishOmission{bad}, prose), p); err == nil {
			t.Error("invalid or author omission accepted", bad)
		}
	}
	if _, err := decodePolish(output(nil, prose), p); err == nil {
		t.Fatal("silent loss of a source passed without disposition")
	}
	p.Targets = []PolishTarget{background}
	if _, err := decodePolish(output([]PolishOmission{omission}, []PolishParagraph{}), p); err != nil {
		t.Fatal("background-only batch must not become a junk paragraph", err)
	}
	// Match the App's atomic removal rule: every source needs a disposition,
	// including a short filler that the server would otherwise silently consume.
	filler := SourceUtterance{ID: uuid.NewString(), Text: "嗯"}
	p.Targets[0].Turns = append(p.Targets[0].Turns, filler)
	if _, err := decodePolish(output([]PolishOmission{omission}, []PolishParagraph{}), p); err == nil {
		t.Fatal("a partially excluded target was accepted for complete removal")
	}
	p.Targets[0].Turns = []SourceUtterance{background.Turns[0]}
	p.Targets[0].CompleteSource = false
	p.Targets[0].RetainedText = "我已经订好洗碗机。"
	if _, err := decodePolish(output([]PolishOmission{omission}, []PolishParagraph{}), p); err == nil {
		t.Fatal("background removal erased unrelated retained prose")
	}
}

func TestNarrativePromptSeparatesSourceIdentityFromSubjectsAndRelevance(t *testing.T) {
	s := polishFixture()
	prepared, err := PrepareRewrite(t.Context(), s, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Instructions string `json:"instructions"`
		Text         struct {
			Format struct {
				Schema map[string]any `json:"schema"`
			} `json:"format"`
		} `json:"text"`
	}
	if json.Unmarshal(prepared.Body, &body) != nil {
		t.Fatal("bad request")
	}
	if _, ok := body.Text.Format.Schema["properties"].(map[string]any)["omissions"]; !ok {
		t.Fatal("model cannot report excluded background")
	}
}

func TestSelectionContextIsBoundedAndOnlyInformsRelevance(t *testing.T) {
	s := polishFixture()
	s.Polish.SelectionContext = &PolishSelectionContext{Opening: "我和老婆正在商量买洗碗机。", Omissions: []PolishOmission{{SourceID: uuid.NewString(), Text: "老张，把办公室的灯关掉。", Reason: "backgroundConversation"}}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareRewrite(t.Context(), s, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input string `json:"input"`
	}
	if json.Unmarshal(prepared.Body, &body) != nil {
		t.Fatal("bad request")
	}
	var input struct {
		SelectionContext *PolishSelectionContext `json:"selectionContext"`
	}
	if json.Unmarshal([]byte(body.Input), &input) != nil || input.SelectionContext == nil || input.SelectionContext.Opening != s.Polish.SelectionContext.Opening {
		t.Fatal("real task lost its ongoing narrative and previous selection")
	}
	s.Polish.SelectionContext.Opening = strings.Repeat("字", 401)
	if s.Validate() == nil {
		t.Fatal("unbounded narrative context accepted")
	}
	s.Polish.SelectionContext.Opening = ""
	prior := s.Polish.SelectionContext.Omissions[0]
	s.Polish.SelectionContext.Omissions = append(s.Polish.SelectionContext.Omissions, prior)
	if s.Validate() == nil {
		t.Fatal("duplicated selection evidence accepted")
	}
}

func TestExcludedSpeechCannotGroundIllustrationSuggestion(t *testing.T) {
	main := PolishTarget{ID: uuid.NewString(), Style: "body", Text: "我想买洗碗机。", SourceText: "我想买洗碗机。", SourceIDs: []string{uuid.NewString()}, CompleteSource: true}
	main.Turns = []SourceUtterance{{ID: main.SourceIDs[0], Text: main.SourceText}}
	background := PolishTarget{ID: uuid.NewString(), Style: "body", Text: "把办公室的灯关掉。", SourceText: "把办公室的灯关掉。", SourceIDs: []string{uuid.NewString()}, CompleteSource: true}
	background.Turns = []SourceUtterance{{ID: background.SourceIDs[0], Text: background.SourceText}}
	p := PolishRequest{Targets: []PolishTarget{main, background}, illustrationSuggestionsEnabled: true}
	output, _ := json.Marshal(map[string]any{
		"paragraphs": []PolishParagraph{{Text: main.Text, Style: "body", TargetIDs: []string{main.ID}}}, "questions": []string{},
		"omissions":              []PolishOmission{{SourceID: background.SourceIDs[0], Text: background.SourceText, Reason: "backgroundConversation"}},
		"illustrationSuggestion": map[string]any{"summary": "办公室关灯", "subject": "办公室亮着一盏灯", "setting": "办公室", "composition": "灯在画面中央", "style": "水彩", "reason": "原话提到了办公室", "sourceQuotes": []string{background.SourceText}},
	})
	result, err := decodePolish(string(output), p)
	if err != nil || result.IllustrationSuggestion != nil {
		t.Fatal("excluded background contaminated image advice", result, err)
	}
}

func TestSituationalInterruptionCanBelongToAuthorOrKnownPerson(t *testing.T) {
	author, wife := uuid.NewString(), uuid.NewString()
	for _, person := range []string{author, wife, ""} {
		turn := SourceUtterance{ID: uuid.NewString(), Text: "哎哟，我脚崴了，好疼。", PersonID: person}
		target := PolishTarget{ID: uuid.NewString(), Text: turn.Text, SourceText: turn.Text, SourceIDs: []string{turn.ID}, Turns: []SourceUtterance{turn}, Style: "body", CompleteSource: true}
		request := PolishRequest{Narrator: &Narrator{PersonID: author, Name: "我"}, Targets: []PolishTarget{target}}
		omission := PolishOmission{SourceID: turn.ID, Text: turn.Text, Reason: "situationalInterruption"}
		raw, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{}, "questions": []string{}, "omissions": []PolishOmission{omission}})
		revision, err := decodePolish(string(raw), request)
		if err != nil || len(revision.Omissions) != 1 || len(revision.Targets) != 1 {
			t.Fatal("identity must not immunize an incidental reaction", person, err)
		}
		request.SelectionContext = &PolishSelectionContext{Opening: "正在讲述工作报告的交付安排。", Omissions: []PolishOmission{omission}}
		if err := request.Validate(); err != nil {
			t.Fatal("continuation lost its previous situational disposition", err)
		}
		omission.Text = "好疼。"
		partial, err := validatePolishOmissions([]PolishOmission{omission}, request)
		if err != nil || partial[turn.ID] {
			t.Fatal("a quoted local phrase must not authorize whole-source removal", err)
		}
		raw, _ = json.Marshal(map[string]any{"paragraphs": []PolishParagraph{}, "questions": []string{}, "omissions": []PolishOmission{omission}})
		if _, err := decodePolish(string(raw), request); err == nil {
			t.Fatal("partial disposition erased the remaining source")
		}
	}
}

func TestSourceGuardAcceptsActualParticleInsertionWithoutSilentlyLosingSources(t *testing.T) {
	first := SourceUtterance{ID: uuid.NewString(), Text: "我在说本周工作。哎哟，我脚崴了，好疼。"}
	second := SourceUtterance{ID: uuid.NewString(), Text: "这让我想起上次受伤的时候，同事帮我做完了报告。我一直很感激，这段也想记下来。"}
	actual := []PolishParagraph{{Text: "我正说着本周的工作，脚突然崴了，疼得厉害。这让我想起上次受伤的时候，同事帮我做完了报告，我一直很感激，这段也想记下来。"}}
	if !polishRetainsSourceAnchors([]SourceUtterance{first, second}, actual) {
		t.Fatal("actual faithful rewrite rejected for inserting 的 and 突然")
	}
	missing := SourceUtterance{ID: uuid.NewString(), Text: "我准备买洗碗机，橱柜要能装十二套餐具。"}
	if polishRetainsSourceAnchors([]SourceUtterance{first, second, missing}, actual) {
		t.Fatal("unrepresented source bypassed the omission disposition")
	}
	if polishRetainsSourceAnchors([]SourceUtterance{first}, []PolishParagraph{{Text: "本周我买了水果。"}}) {
		t.Fatal("one generic partial anchor is insufficient")
	}
}

func TestLocalSituationalOmissionKeepsMainlineAndCannotRemoveAppSource(t *testing.T) {
	author := uuid.NewString()
	turn := SourceUtterance{ID: uuid.NewString(), PersonID: author, Text: "报告周五交。哎哟，我脚崴了，好疼。接着说报告，明天下午发草稿。"}
	target := PolishTarget{ID: uuid.NewString(), Text: turn.Text, SourceText: turn.Text, SourceIDs: []string{turn.ID}, Turns: []SourceUtterance{turn}, Style: "body", CompleteSource: true}
	p := PolishRequest{Narrator: &Narrator{PersonID: author, Name: "我"}, Targets: []PolishTarget{target}}
	local := PolishOmission{SourceID: turn.ID, Text: "哎哟，我脚崴了，好疼。", Reason: "situationalInterruption"}
	output := func(omissions []PolishOmission, paragraphs []PolishParagraph) string {
		b, _ := json.Marshal(map[string]any{"paragraphs": paragraphs, "questions": []string{}, "omissions": omissions})
		return string(b)
	}
	prose := []PolishParagraph{{TargetIDs: []string{target.ID}, Style: "body", Text: "报告周五交，明天下午发草稿。"}}
	result, err := decodePolish(output([]PolishOmission{local}, prose), p)
	if err != nil || len(result.Omissions) != 0 || !result.Targets[0].Turns[0].Equal(turn) {
		t.Fatal("local selection must retain the App source and immutable evidence", err)
	}
	if _, err := decodePolish(output([]PolishOmission{local}, nil), p); err == nil {
		t.Fatal("local omission erased the rest of the source")
	}
	if _, err := decodePolish(output([]PolishOmission{local, local}, prose), p); err == nil {
		t.Fatal("duplicate local exclusion accepted")
	}
	local.Text = "没说过的插话"
	if _, err := decodePolish(output([]PolishOmission{local}, prose), p); err == nil {
		t.Fatal("ungrounded local exclusion accepted")
	}
	turn.Text = "好疼。报告周五交。好疼。"
	target.Turns[0] = turn
	p.Targets[0] = target
	local.Text = "好疼。"
	if _, err := decodePolish(output([]PolishOmission{local}, prose), p); err == nil {
		t.Fatal("ambiguous repeated excerpt accepted")
	}
}
