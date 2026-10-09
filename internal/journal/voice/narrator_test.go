package voice

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"slices"
	"strings"
	"testing"
)

func TestAuthorAndConfirmedSpeakerIDsReachOrdinaryAndStructuredModels(t *testing.T) {
	s := polishFixture()
	author, wife := uuid.NewString(), uuid.NewString()
	s.Narrator = &Narrator{PersonID: author, Name: "我"}
	s.Polish.Narrator = s.Narrator
	turn := SourceUtterance{ID: s.Polish.Targets[0].SourceIDs[0], Text: "我昨天在医院值班，很累。", PersonID: wife, Person: "妻子", Speaker: "segment:1"}
	s.Polish.Targets[0].Turns = []SourceUtterance{turn}
	prepared, err := PrepareRewrite(context.Background(), s, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	var input struct {
		Narrator *Narrator `json:"narrator"`
		Targets  []struct {
			Turns []SourceUtterance `json:"turns"`
		} `json:"targets"`
	}
	if err = json.Unmarshal([]byte(body["input"].(string)), &input); err != nil {
		t.Fatal(err)
	}
	if !sameNarrator(input.Narrator, s.Narrator) || input.Targets[0].Turns[0].PersonID != wife {
		t.Fatal("lost author or confirmed speaker identity")
	}
	if !strings.Contains(body["instructions"].(string), narratorInstructions) {
		t.Fatal("author perspective rules omitted")
	}
	output := `{"paragraphs":[{"targetIDs":["` + s.Polish.Targets[0].ID + `"],"style":"body","text":"妻子昨天在医院值班，很累。"}],"questions":[]}`
	revision, err := decodePolish(output, *s.Polish)
	if err != nil {
		t.Fatal(err)
	}
	if !sameNarrator(revision.Narrator, s.Narrator) {
		t.Fatal("server did not echo trusted author baseline")
	}
	s.Polish.Narrator = &Narrator{PersonID: wife, Name: "妻子"}
	s.Polish = nil
	s.PendingUtterances = []SourceUtterance{turn}
	data, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var structured rewriteModelDocument
	if err = json.Unmarshal(data, &structured); err != nil {
		t.Fatal(err)
	}
	if !sameNarrator(structured.Narrator, s.Narrator) || structured.PendingUtterances[0].PersonID != wife {
		t.Fatal("structural command path loses identity")
	}
}

func TestInvalidAuthorOrSpeakerIdentityIsRejected(t *testing.T) {
	s := polishFixture()
	for _, author := range []*Narrator{{PersonID: "provider:1", Name: "我"}, {PersonID: uuid.NewString(), Name: " "}} {
		s.Polish.Narrator = author
		if s.Polish.Validate() == nil {
			t.Fatal("invalid author accepted")
		}
	}
	s.Polish.Narrator = nil
	s.Polish.Targets[0].Turns = []SourceUtterance{{ID: s.Polish.Targets[0].SourceIDs[0], PersonID: "1", Text: "原话"}}
	if s.Polish.Validate() == nil {
		t.Fatal("provider label mistaken for person identity")
	}
}

func TestIdentityCorrectionUsesOriginalTurnsInsteadOfOldGeneratedSubject(t *testing.T) {
	s := polishFixture()
	target := &s.Polish.Targets[0]
	target.IdentityCorrection, target.CompleteSource = true, true
	target.Text, target.RetainedText = "妻子在医院值班。", "妻子在医院值班。"
	target.SourceText = "我在医院值班。"
	target.Turns = []SourceUtterance{{ID: target.SourceIDs[0], Text: target.SourceText, PersonID: uuid.NewString(), Person: "小林"}}
	prepared, err := PrepareRewrite(context.Background(), s, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body["input"].(string), "妻子") || !strings.Contains(body["input"].(string), "小林") {
		t.Fatal("old generated attribution contaminated identity correction evidence")
	}
	target.CompleteSource = false
	prepared, err = PrepareRewrite(context.Background(), s, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body["input"].(string), target.RetainedText) {
		t.Fatal("bounded source window dropped unsourced retained facts")
	}
}

func TestConfirmedSelfReportDoesNotInventGenderThroughPunctuation(t *testing.T) {
	s := polishFixture()
	s.Polish.Narrator = &Narrator{PersonID: uuid.NewString(), Name: "我"}
	target := &s.Polish.Targets[0]
	target.CompleteSource = true
	target.Turns = []SourceUtterance{{ID: target.SourceIDs[0], PersonID: uuid.NewString(), Person: "小林", Text: "我昨天在医院值班，很累。"}}
	for _, text := range []string{"小林说他昨天在医院值班，很累。", "小林说，他昨天在医院值班，很累。", "昨天我去公园。小林表示：她昨天在医院值班，很累。"} {
		got := polishConfirmedSelfReferences(text, []string{target.ID}, *s.Polish)
		if strings.ContainsAny(got, "他她") || !strings.Contains(got, "小林昨天在医院值班，很累。") {
			t.Fatalf("unsupported subject: %s", got)
		}
	}
	quote := "小林说：“他昨天在医院值班。”"
	if got := polishConfirmedSelfReferences(quote, []string{target.ID}, *s.Polish); got != quote {
		t.Fatal("changed quoted speech")
	}
	for _, text := range []string{"我们见到了小林，他今天从杭州坐火车过来。", "小林做了自我介绍，说他昨天在医院值班。"} {
		if got := polishConfirmedSelfReferences(text, []string{target.ID}, *s.Polish); strings.ContainsAny(got, "他她") {
			t.Fatal("invented actor gender survived a sentence-internal clause", got)
		}
	}
	neutral := "小林今天帮助他人。"
	if got := polishConfirmedSelfReferences(neutral, []string{target.ID}, *s.Polish); got != neutral {
		t.Fatal("changed a word containing a pronoun character")
	}
	mixed := *s.Polish
	mixed.Targets = slices.Clone(mixed.Targets)
	mixed.Targets[0].Turns = append(slices.Clone(target.Turns), SourceUtterance{ID: uuid.NewString(), PersonID: s.Polish.Narrator.PersonID, Person: "我", Text: "我今天也休息。"})
	text := "小林今天休息，他今天还去了公园。"
	if got := polishConfirmedSelfReferences(text, []string{target.ID}, mixed); got != text {
		t.Fatal("mixed actors must not be replaced using a single-person rule")
	}
	target.Turns[0].Text = "他昨天在医院值班，很累，我今天休息。"
	third := "小林说，他昨天在医院值班，很累。"
	if got := polishConfirmedSelfReferences(third, []string{target.ID}, *s.Polish); got != third {
		t.Fatal("changed a source-supported third-person reference")
	}
	target.Turns[0].Text = "我昨天在医院值班，很累。"
	target.CompleteSource = false
	if got := polishConfirmedSelfReferences(third, []string{target.ID}, *s.Polish); got != third {
		t.Fatal("incomplete evidence changed retained prose")
	}
}

func TestConfirmedReciprocalAuthorReferenceUsesNarratorWithoutChangingQuotesOrOtherNames(t *testing.T) {
	s := polishFixture()
	s.Polish.Narrator = &Narrator{PersonID: uuid.NewString(), Name: "老公宝"}
	target := &s.Polish.Targets[0]
	target.CompleteSource = true
	target.Turns = []SourceUtterance{{ID: target.SourceIDs[0], PersonID: uuid.NewString(), Person: "老婆宝", Text: "我和我老公宝出来很开心。"}}
	for _, text := range []string{"老婆宝也说，和我老公宝一起出来很开心。", "老婆宝表示：跟我的老公宝出来很开心。"} {
		got := polishConfirmedAuthorReferences(text, []string{target.ID}, *s.Polish)
		if strings.Contains(got, "我老公宝") || strings.Contains(got, "我的老公宝") || !strings.Contains(got, "我") {
			t.Fatal("reciprocal author perspective not corrected", got)
		}
	}
	for _, text := range []string{"老婆宝说：“和我老公宝出来很开心。”", "小林说，和我老公宝出来很开心。", "我和我老公宝出来很开心。"} {
		if got := polishConfirmedAuthorReferences(text, []string{target.ID}, *s.Polish); got != text {
			t.Fatal("ungrounded/quoted subject was transformed", got)
		}
	}
	target.CompleteSource = false
	text := "老婆宝也说，和我老公宝一起出来很开心。"
	if got := polishConfirmedAuthorReferences(text, []string{target.ID}, *s.Polish); got != text {
		t.Fatal("incomplete source changed narrator reference")
	}
}

func TestOrdinaryContinuationUsesRawActorsInsteadOfOldGeneratedAttribution(t *testing.T) {
	s := polishFixture()
	s.Polish.Narrator = &Narrator{PersonID: uuid.NewString(), Name: "老公宝"}
	target := &s.Polish.Targets[0]
	target.CompleteSource = true
	target.Text, target.RetainedText = "老婆宝吃了两碗饭，我把剩菜装进饭盒。", "老婆宝吃了两碗饭，我把剩菜装进饭盒。"
	target.SourceText = "我吃了两碗饭，还把剩菜装进饭盒。"
	target.Turns = []SourceUtterance{{ID: target.SourceIDs[0], Text: target.SourceText, PersonID: uuid.NewString(), Person: "老婆宝"}}
	check := func() map[string]any {
		t.Helper()
		prepared, err := PrepareRewrite(context.Background(), s, 1, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err = json.Unmarshal(prepared.Body, &body); err != nil {
			t.Fatal(err)
		}
		var input struct {
			Targets []map[string]any `json:"targets"`
		}
		if err = json.Unmarshal([]byte(body["input"].(string)), &input); err != nil {
			t.Fatal(err)
		}
		return input.Targets[0]
	}
	input := check()
	if input["text"] != "" || input["sourceText"] != "" || input["retainedText"] != "" {
		t.Fatal("ordinary continuation reinforced a generated subject error")
	}
	turn := input["turns"].([]any)[0].(map[string]any)
	if turn["narrativeRole"] != "other" || turn["person"] != "老婆宝" || turn["text"] != target.SourceText {
		t.Fatal("lost explicit other actor")
	}
	prepared, err := PrepareRewrite(context.Background(), s, 1, "fixture")
	if err != nil || prepared.Parameters.ReasoningEffort != "disabled" || prepared.Parameters.MaxOutputTokens > 4096 || prepared.TimeoutSeconds != 90 {
		t.Fatal("multi-person perspective lacks bounded semantic reasoning", err)
	}
	policy := promptconfig.Defaults("fixture", "fixture", "fixture", 35)["journal"]
	ctx := promptconfig.WithRevision(context.Background(), promptconfig.Revision{ID: "lower-bound", Scope: "journal", Policy: policy})
	limited, err := PrepareRewrite(ctx, s, 1, "fixture")
	if err != nil || limited.TimeoutSeconds != 35 {
		t.Fatal("multi-person reasoning bypassed the configured timeout", err)
	}
	target.Turns[0].PersonID = s.Polish.Narrator.PersonID
	if check()["turns"].([]any)[0].(map[string]any)["narrativeRole"] != "author" {
		t.Fatal("author actor not derived from confirmed identity")
	}
	target.Turns[0].PersonID = ""
	if check()["turns"].([]any)[0].(map[string]any)["narrativeRole"] != "unknown" {
		t.Fatal("unknown actor guessed")
	}
	target.CompleteSource = false
	if check()["retainedText"] != target.RetainedText {
		t.Fatal("incomplete evidence removed retained facts")
	}
}
