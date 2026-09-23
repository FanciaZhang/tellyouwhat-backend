package voice

import (
	"github.com/google/uuid"
	"testing"
)

func TestExplicitFormatCommandCanStyleUserOwnedTextWithSourceEvidence(t *testing.T) {
	block, source := uuid.NewString(), uuid.NewString()
	s := Snapshot{Revision: 2, Blocks: []Block{{ID: block, Text: "今天很好。", Style: "body"}},
		EditedBlockIDs: []string{block}, PendingUtterances: []SourceUtterance{{ID: source, Text: "刚才那句话加粗"}}}
	command := FormatCommand{ID: uuid.NewString(), BlockID: block, SourceID: source,
		Anchor: TextAnchor{Quote: "今天很好。"}, Instruction: "刚才那句话加粗", Mark: "bold", Enabled: true}
	r := Revision{BaseRevision: 2, FormatCommands: []FormatCommand{command}, ConsumedSourceIDs: []string{source}}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*FormatCommand){
		func(c *FormatCommand) { c.Instruction = "伪造指令" },
		func(c *FormatCommand) { c.SourceID = uuid.NewString() },
		func(c *FormatCommand) { c.BlockID = uuid.NewString() },
		func(c *FormatCommand) { c.Mark = "execute" },
		func(c *FormatCommand) { c.Anchor.Quote = "" },
	} {
		bad := command
		mutate(&bad)
		r.FormatCommands = []FormatCommand{bad}
		if r.Validate(s) == nil {
			t.Fatalf("accepted invalid command: %+v", bad)
		}
	}
	r.FormatCommands = []FormatCommand{command, command}
	if r.Validate(s) == nil {
		t.Fatal("duplicate command IDs")
	}
	r.FormatCommands = []FormatCommand{command}
	s.MediaOnlyBlockIDs = []string{block}
	if r.Validate(s) == nil {
		t.Fatal("formatted media-only block")
	}
}

func TestFormatCommandSchemaIsClosedAndRequired(t *testing.T) {
	schema := voiceRevisionSchema()
	required := schema["required"].([]string)
	found := false
	for _, key := range required {
		if key == "formatCommands" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing required commands array")
	}
	properties := schema["properties"].(map[string]any)
	items := properties["formatCommands"].(map[string]any)["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Fatal("open command schema")
	}
}
