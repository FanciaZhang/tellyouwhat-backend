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
