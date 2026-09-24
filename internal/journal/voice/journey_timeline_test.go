package voice

import (
	"github.com/google/uuid"
	"testing"
)

func TestJourneyAndTimelineShareExactSourceWithoutDoubleConsumption(t *testing.T) {
	source, timelineBlock, mapBlock := uuid.NewString(), uuid.NewString(), uuid.NewString()
	content, instruction := "早上去河边散步。", "生成时间线和地图。"
	s := Snapshot{PendingUtterances: []SourceUtterance{{ID: source, Text: content + instruction}}}
	evidence := TableSource{SourceID: source, Anchor: TextAnchor{Quote: content}}
	timeline := TimelineCreation{ID: uuid.NewString(), BlockID: timelineBlock, TimelineID: uuid.NewString(), SourceID: source, Instruction: instruction, Title: "一天", Events: []TimelineEvent{{ID: uuid.NewString(), Title: "河边散步", TimeExpression: "早上", Precision: "period", Period: "morning", Intent: "experience", Sources: []TableSource{evidence}}}}
	journey := JourneyCreation{ID: uuid.NewString(), BlockID: mapBlock, MapID: uuid.NewString(), SourceID: source, Instruction: instruction, Title: "散步地图", Visits: []JourneyVisit{{ID: uuid.NewString(), Expression: "河边", SourceID: source, Anchor: evidence.Anchor}}}
	r := Revision{ConsumedSourceIDs: []string{source}, TimelineCreations: []TimelineCreation{timeline}, JourneyCreations: []JourneyCreation{journey}, SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: content, Role: "content", BlockIDs: []string{timelineBlock, mapBlock}}, {Text: instruction, Role: "instruction", BlockIDs: []string{timelineBlock, mapBlock}}}}}}
	if err := r.Validate(s); err != nil {
		t.Fatalf("joint proposal rejected: %v", err)
	}
	for _, role := range []int{0, 1} {
		original := r.SourcePartitions[0].Segments[role].BlockIDs
		r.SourcePartitions[0].Segments[role].BlockIDs = append(append([]string{}, original...), uuid.NewString())
		if err := r.Validate(s); err == nil {
			t.Fatal("unrelated target authorized by shared source")
		}
		r.SourcePartitions[0].Segments[role].BlockIDs = original
	}
	r.ConsumedSourceIDs = append(r.ConsumedSourceIDs, source)
	if err := r.Validate(s); err == nil {
		t.Fatal("source consumed twice")
	}
}
