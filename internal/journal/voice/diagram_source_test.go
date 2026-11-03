package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"reflect"
	"strings"
	"testing"
)

func TestDiagramHistoricalContextSnapshotAndModelWire(t *testing.T) {
	s := journeyContextFixture()
	id := uuid.NewString()
	s.KnownSourceIDs = append(s.KnownSourceIDs, id)
	s.DiagramSourceContext = []TableSource{{SourceID: id, Anchor: TextAnchor{Quote: "主题是周末散步。"}}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	data, err := rewriteModelInput(decoded, 1)
	if err != nil {
		t.Fatal(err)
	}
	var model rewriteModelDocument
	if err := json.Unmarshal(data, &model); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(model.DiagramSourceContext, s.DiagramSourceContext) {
		t.Fatal("diagram evidence lost during projection")
	}
	for name, mutate := range map[string]func(*Snapshot){
		"unknown source": func(s *Snapshot) { s.DiagramSourceContext[0].SourceID = uuid.NewString() },
		"adjacent text":  func(s *Snapshot) { s.DiagramSourceContext[0].Anchor.Suffix = "这段加粗" },
		"duplicate":      func(s *Snapshot) { s.DiagramSourceContext = append(s.DiagramSourceContext, s.DiagramSourceContext[0]) },
		"budget":         func(s *Snapshot) { s.DiagramSourceContext[0].Anchor.Quote = strings.Repeat("文", 6001) },
	} {
		t.Run(name, func(t *testing.T) {
			var invalid Snapshot
			if err := json.Unmarshal(wire, &invalid); err != nil {
				t.Fatal(err)
			}
			mutate(&invalid)
			if invalid.Validate() == nil {
				t.Fatal("invalid diagram evidence accepted by request validation")
			}
		})
	}
}

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
		if got := structuredSourceAuthorized(TableSource{SourceID: id, Anchor: TextAnchor{Quote: c.quote}}, c.role, block, r, s, nil); got != c.want {
			t.Errorf("%s/%s got %v", c.quote, c.role, got)
		}
	}
	source := TableSource{SourceID: id, Anchor: TextAnchor{Quote: content}}
	if structuredSourceAuthorized(source, "content", uuid.NewString(), r, s, nil) {
		t.Fatal("wrong target accepted")
	}
	r.SourcePartitions = append(r.SourcePartitions, r.SourcePartitions[0])
	if structuredSourceAuthorized(source, "content", block, r, s, nil) {
		t.Fatal("duplicate partition accepted")
	}
}

func TestDiagramHistoricalSourceNeverAuthorizesInstruction(t *testing.T) {
	id, block := uuid.NewString(), uuid.NewString()
	source := TableSource{SourceID: id, Anchor: TextAnchor{Quote: "周末去散步。"}}
	s := Snapshot{KnownSourceIDs: []string{id}}
	history := []TableSource{source}
	if !structuredSourceAuthorized(source, "content", block, Revision{}, s, history) {
		t.Fatal("historical content rejected")
	}
	if structuredSourceAuthorized(source, "instruction", block, Revision{}, s, history) {
		t.Fatal("historical instruction authorized")
	}
	if structuredSourceAuthorized(source, "content", block, Revision{ConsumedSourceIDs: []string{id}}, s, history) {
		t.Fatal("old source consumed again")
	}
	if structuredSourceAuthorized(source, "content", block, Revision{}, Snapshot{}, history) {
		t.Fatal("unknown historical source accepted")
	}
	s.PendingUtterances = []SourceUtterance{{ID: id, Text: source.Anchor.Quote}}
	if structuredSourceAuthorized(source, "content", block, Revision{}, s, history) {
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
