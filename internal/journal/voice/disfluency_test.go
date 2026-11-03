package voice

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPolishDisfluencyHasGroundedDispositionForAnySpeaker(t *testing.T) {
	author := uuid.NewString()
	main := SourceUtterance{ID: uuid.NewString(), PersonID: author, Text: "这个能力还在讨论，不是已经实现了。"}
	filler := SourceUtterance{ID: uuid.NewString(), PersonID: author, Text: "嗯。"}
	fragment := SourceUtterance{ID: uuid.NewString(), Text: "这么一个广泛的一个。"}
	p := PolishRequest{Narrator: &Narrator{PersonID: author, Name: "我"}}
	for _, turn := range []SourceUtterance{main, filler, fragment} {
		p.Targets = append(p.Targets, PolishTarget{ID: uuid.NewString(), Text: turn.Text, SourceText: turn.Text, Style: "body", CompleteSource: true, SourceIDs: []string{turn.ID}, Turns: []SourceUtterance{turn}})
	}
	omissions := []PolishOmission{{SourceID: filler.ID, Text: filler.Text, Reason: "speechDisfluency"}, {SourceID: fragment.ID, Text: fragment.Text, Reason: "speechDisfluency"}}
	output := func(items []PolishOmission) string {
		data, _ := json.Marshal(map[string]any{"omissions": items, "paragraphs": []PolishParagraph{{Text: main.Text, Style: "body", TargetIDs: []string{p.Targets[0].ID}}}, "questions": []string{}})
		return string(data)
	}
	revision, err := decodePolish(output(omissions), p)
	if err != nil || len(revision.Omissions) != 2 {
		t.Fatal("meaningless speech must not reject substantive prose", err)
	}
	bad := append([]PolishOmission{}, omissions...)
	bad[0].Reason = "recognitionNoise"
	if _, err := decodePolish(output(bad), p); polishValidationCode(err) != "omission_author_role" {
		t.Fatal("author must not become unrecognized noise", err)
	}
	bad[0] = omissions[0]
	bad[0].Text = "嗯"
	partial, err := decodePolish(output(bad), p)
	if err == nil || partial != nil {
		t.Fatal("partial filler exclusion cannot remove a whole target")
	}
	bad[0] = omissions[0]
	bad[0].SourceID = uuid.NewString()
	if _, err := decodePolish(output(bad), p); polishValidationCode(err) != "omission_source" {
		t.Fatal("unknown source was accepted", err)
	}
	bad[0] = omissions[0]
	bad[0].Text = "哦。"
	if _, err := decodePolish(output(bad), p); polishValidationCode(err) != "target_coverage" {
		t.Fatal("a local note erased an uncovered target", err)
	}
	if _, err := decodePolish(output(nil), p); polishValidationCode(err) != "target_coverage" {
		t.Fatal("silent loss of substantive targets was accepted", err)
	}
	snapshot := Snapshot{WritingStyle: "natural", DictationMode: true, Narrator: p.Narrator, Polish: &p}
	for _, target := range p.Targets {
		snapshot.Blocks = append(snapshot.Blocks, Block{ID: target.ID, Text: target.Text, Style: target.Style})
	}
	prepared, err := PrepareRewrite(t.Context(), snapshot, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Text struct {
			Format struct{ Schema map[string]any }
		}
	}
	if json.Unmarshal(prepared.Body, &body) != nil {
		t.Fatal("invalid model contract")
	}
	properties := body.Text.Format.Schema["properties"].(map[string]any)
	sourceIDs := properties["omissions"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["sourceID"].(map[string]any)["enum"]
	encodedIDs, _ := json.Marshal(sourceIDs)
	if !strings.Contains(string(encodedIDs), filler.ID) {
		t.Fatal("pure filler source missing from real output contract")
	}
}

func TestPolishDisfluencyDoesNotRequireVerbatimFillersInsideRetainedSpeech(t *testing.T) {
	turn := SourceUtterance{ID: uuid.NewString(), Text: "嗯，我还没想清楚。嗯，可能明天才确定。"}
	target := PolishTarget{ID: uuid.NewString(), Text: turn.Text, SourceText: turn.Text, Style: "body", CompleteSource: true, SourceIDs: []string{turn.ID}, Turns: []SourceUtterance{turn}}
	data, _ := json.Marshal(map[string]any{"omissions": []PolishOmission{}, "paragraphs": []PolishParagraph{{Text: "我还没想清楚，可能明天才确定。", Style: "body", TargetIDs: []string{target.ID}}}, "questions": []string{}})
	if _, err := decodePolish(string(data), PolishRequest{Targets: []PolishTarget{target}}); err != nil {
		t.Fatal("ordinary linguistic editing failed", err)
	}
}

func TestPolishAcknowledgmentCanContributeWithoutVerbatimFiller(t *testing.T) {
	main := SourceUtterance{ID: uuid.NewString(), Text: "我计划周五交报告。"}
	acknowledgment := SourceUtterance{ID: uuid.NewString(), Text: "嗯，对。", Person: "老婆宝", PersonID: uuid.NewString()}
	p := PolishRequest{}
	for _, turn := range []SourceUtterance{main, acknowledgment} {
		p.Targets = append(p.Targets, PolishTarget{ID: uuid.NewString(), Text: turn.Text, SourceText: turn.Text, Style: "body", CompleteSource: true, SourceIDs: []string{turn.ID}, Turns: []SourceUtterance{turn}})
	}
	output := func(ids []string) string {
		data, _ := json.Marshal(map[string]any{"omissions": []PolishOmission{}, "paragraphs": []PolishParagraph{{Text: "我计划周五交报告，老婆宝也赞同这个安排。", Style: "body", TargetIDs: ids}}, "questions": []string{}})
		return string(data)
	}
	if _, err := decodePolish(output([]string{p.Targets[0].ID, p.Targets[1].ID}), p); err != nil {
		t.Fatal("faithful acknowledgment was rejected", err)
	}
	if _, err := decodePolish(output([]string{p.Targets[0].ID}), p); polishValidationCode(err) != "target_coverage" {
		t.Fatal("acknowledgment lost its source link", err)
	}
	for _, text := range []string{"明天", "不对", "别去", "我想买一台电脑"} {
		if !polishNeedsLexicalAnchor(text) {
			t.Fatal("substantive short speech lost its guard", text)
		}
	}
}

func TestLocalDisfluencyNotesCannotDeleteSubstantiveSources(t *testing.T) {
	turn := SourceUtterance{ID: uuid.NewString(), Text: "嗯，我还没想清楚。嗯，可能明天才确定。"}
	target := PolishTarget{ID: uuid.NewString(), Text: turn.Text, SourceText: turn.Text, Style: "body", CompleteSource: true, SourceIDs: []string{turn.ID}, Turns: []SourceUtterance{turn}}
	p := PolishRequest{Targets: []PolishTarget{target}}
	note := PolishOmission{SourceID: turn.ID, Text: "嗯，嗯，", Reason: "speechDisfluency"}
	raw := func(paragraphs []PolishParagraph, item PolishOmission) string {
		data, _ := json.Marshal(map[string]any{"omissions": []PolishOmission{item}, "paragraphs": paragraphs, "questions": []string{}})
		return string(data)
	}
	prose := []PolishParagraph{{Text: "我还没想清楚，可能明天才确定。", Style: "body", TargetIDs: []string{target.ID}}}
	revision, err := decodePolish(raw(prose, note), p)
	if err != nil || len(revision.Omissions) != 0 {
		t.Fatal("local language note incorrectly became a deletion action", err)
	}
	if _, err := decodePolish(raw([]PolishParagraph{}, note), p); err == nil {
		t.Fatal("local note authorized whole-source deletion")
	}
	excessive := make([]PolishOmission, 193)
	for i := range excessive {
		excessive[i] = note
	}
	encoded, _ := json.Marshal(map[string]any{"omissions": excessive, "paragraphs": prose, "questions": []string{}})
	if _, err := decodePolish(string(encoded), p); err == nil {
		t.Fatal("local notes bypassed the response size bound")
	}
	note.SourceID = uuid.NewString()
	if _, err := decodePolish(raw(prose, note), p); polishValidationCode(err) != "omission_source" {
		t.Fatal("unknown reference bypassed validation", err)
	}
	note.SourceID = turn.ID
	note.Reason = "situationalInterruption"
	if _, err := decodePolish(raw(prose, note), p); polishValidationCode(err) != "omission_quote" {
		t.Fatal("nonliteral relevance quote bypassed validation", err)
	}
}
