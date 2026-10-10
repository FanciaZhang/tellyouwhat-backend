package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"testing"
)

func TestOneSpeakerCanReportAnotherPersonsActions(t *testing.T) {
	s := polishFixture()
	s.Polish.Narrator = &Narrator{PersonID: uuid.NewString(), Name: "我"}
	target := &s.Polish.Targets[0]
	target.CompleteSource = true
	target.Turns = []SourceUtterance{{ID: target.SourceIDs[0], PersonID: uuid.NewString(), Person: "小林", Text: "我叫小林。我昨天帮小王打了报告。小王有紧急任务，今天还要加班。"}}
	text := "小林昨天帮小王打了报告。小王有紧急任务，他今天还要加班。"
	output, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: text, Style: "body", TargetIDs: []string{target.ID}}}, "questions": []string{}})
	result, err := decodePolish(string(output), *s.Polish)
	if err != nil || len(result.Paragraphs) != 1 || result.Paragraphs[0].Text != text {
		t.Fatalf("source speaker is not the only action subject: %+v %v", result, err)
	}
}
