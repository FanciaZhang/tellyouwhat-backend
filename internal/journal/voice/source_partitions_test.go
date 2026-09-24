package voice

import (
	"github.com/google/uuid"
	"testing"
)

func TestSourcePartitionsPreserveFullUtteranceAndDistinctBodyLinks(t *testing.T) {
	source, heading, body := uuid.NewString(), uuid.NewString(), uuid.NewString()
	s := Snapshot{PendingUtterances: []SourceUtterance{{ID: source, Text: "主题是隐私。数据属于用户。"}}}
	r := Revision{Passages: []Passage{{BlockID: heading, SourceIDs: []string{source}}, {BlockID: body, SourceIDs: []string{source}}},
		SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{
			{Text: "主题是", Role: "organization", BlockIDs: []string{heading}},
			{Text: "隐私。", Role: "content", BlockIDs: []string{heading}},
			{Text: "数据属于用户。", Role: "content", BlockIDs: []string{body}},
		}}}}
	if err := validateSourcePartitions(r, s); err != nil {
		t.Fatal(err)
	}
	original := r.SourcePartitions[0].Segments[2]
	for _, invalid := range []SourceSegment{
		{Text: "数据属于用户", Role: "content", BlockIDs: []string{body}},
		{Text: original.Text, Role: "content", BlockIDs: []string{uuid.NewString()}},
		{Text: original.Text, Role: "context"},
		{Text: original.Text, Role: "correction", BlockIDs: []string{body}},
	} {
		r.SourcePartitions[0].Segments[2] = invalid
		if validateSourcePartitions(r, s) == nil {
			t.Fatalf("accepted invalid partition: %+v", invalid)
		}
	}
	r.SourcePartitions[0].Segments[2] = original
	r.SourcePartitions = append(r.SourcePartitions, r.SourcePartitions[0])
	if validateSourcePartitions(r, s) == nil {
		t.Fatal("duplicate source")
	}
}

func TestSourcePartitionsCannotHideInstruction(t *testing.T) {
	source, block := uuid.NewString(), uuid.NewString()
	s := Snapshot{PendingUtterances: []SourceUtterance{{ID: source, Text: "这句标黄"}}}
	r := Revision{FormatCommands: []FormatCommand{{SourceID: source, BlockID: block, Instruction: "这句标黄"}},
		SourcePartitions: []SourcePartition{{SourceID: source, Segments: []SourceSegment{{Text: "这句标黄", Role: "instruction", BlockIDs: []string{block}}}}}}
	if err := validateSourcePartitions(r, s); err != nil {
		t.Fatal(err)
	}
	r.SourcePartitions[0].Segments[0].Role = "context"
	r.SourcePartitions[0].Segments[0].BlockIDs = nil
	if validateSourcePartitions(r, s) == nil {
		t.Fatal("hidden instruction")
	}
	found := false
	for _, key := range voiceRevisionSchema()["required"].([]string) {
		if key == "sourcePartitions" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing required wire field")
	}
}
