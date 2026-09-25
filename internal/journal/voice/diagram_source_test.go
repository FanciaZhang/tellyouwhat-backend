package voice

import (
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestDiagramSourceAuthorization(t *testing.T) {
	id, block := uuid.NewString(), uuid.NewString()
	content, instruction := "周末去散步。", "整理成思维导图。"
	s := Snapshot{PendingUtterances: []SourceUtterance{{ID: id, Text: content + instruction}}}
	r := Revision{ConsumedSourceIDs: []string{id}, SourcePartitions: []SourcePartition{{SourceID: id, Segments: []SourceSegment{
		{Text: content, Role: "content", BlockIDs: []string{block}}, {Text: instruction, Role: "instruction", BlockIDs: []string{block}},
	}}}}
	for _, c := range []struct {
		quote, role string
		want        bool
	}{
		{content, "content", true}, {instruction, "instruction", true},
		{instruction, "content", false}, {content, "instruction", false},
		{"散步。整理", "content", false}, {"整理成", "instruction", false},
	} {
		if got := diagramSourceAuthorized(TableSource{SourceID: id, Anchor: TextAnchor{Quote: c.quote}}, c.role, block, r, s, nil); got != c.want {
			t.Errorf("%s/%s got %v", c.quote, c.role, got)
		}
	}
	source := TableSource{SourceID: id, Anchor: TextAnchor{Quote: content}}
	if diagramSourceAuthorized(source, "content", uuid.NewString(), r, s, nil) {
		t.Fatal("wrong target accepted")
	}
	r.SourcePartitions = append(r.SourcePartitions, r.SourcePartitions[0])
	if diagramSourceAuthorized(source, "content", block, r, s, nil) {
		t.Fatal("duplicate partition accepted")
	}
}

func TestDiagramHistoricalSourceNeverAuthorizesInstruction(t *testing.T) {
	id, block := uuid.NewString(), uuid.NewString()
	source := TableSource{SourceID: id, Anchor: TextAnchor{Quote: "周末去散步。"}}
	s := Snapshot{KnownSourceIDs: []string{id}}
	history := []TableSource{source}
	if !diagramSourceAuthorized(source, "content", block, Revision{}, s, history) {
		t.Fatal("historical content rejected")
	}
	if diagramSourceAuthorized(source, "instruction", block, Revision{}, s, history) {
		t.Fatal("historical instruction authorized")
	}
	if diagramSourceAuthorized(source, "content", block, Revision{ConsumedSourceIDs: []string{id}}, s, history) {
		t.Fatal("old source consumed again")
	}
	if diagramSourceAuthorized(source, "content", block, Revision{}, Snapshot{}, history) {
		t.Fatal("unknown historical source accepted")
	}
	s.PendingUtterances = []SourceUtterance{{ID: id, Text: source.Anchor.Quote}}
	if diagramSourceAuthorized(source, "content", block, Revision{}, s, history) {
		t.Fatal("pending source disguised as history")
	}
}

func TestStructuredSourceContextBounds(t *testing.T) {
	id := uuid.NewString()
	s := Snapshot{KnownSourceIDs: []string{id}}
	source := TableSource{SourceID: id, Anchor: TextAnchor{Quote: strings.Repeat("文", 6000)}}
	if validateStructuredSourceContext([]TableSource{source}, s) != nil {
		t.Fatal("boundary rejected")
	}
	source.Anchor.Quote += "文"
	if validateStructuredSourceContext([]TableSource{source}, s) == nil {
		t.Fatal("oversized context accepted")
	}
	source.Anchor.Quote = "正文"
	if validateStructuredSourceContext([]TableSource{source, source}, s) == nil {
		t.Fatal("duplicate quote accepted")
	}
	source.Anchor.Prefix = "历史指令"
	if validateStructuredSourceContext([]TableSource{source}, s) == nil {
		t.Fatal("adjacent private context accepted")
	}
}
