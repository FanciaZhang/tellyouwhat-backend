package voice

import (
	"encoding/json"
	"github.com/google/uuid"
	"os"
	"testing"
)

func spokenCommandFixture(t *testing.T, name string) (Snapshot, Revision) {
	t.Helper()
	var s Snapshot
	var r Revision
	for suffix, value := range map[string]any{"snapshot": &s, "rejected": &r} {
		raw, err := os.ReadFile("testdata/spoken_commands/" + name + "-" + suffix + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, value); err != nil {
			t.Fatal(err)
		}
	}
	return s, r
}

func TestRealRejectedStructuresCanonicalizeWithoutChangingFacts(t *testing.T) {
	for _, name := range []string{"table", "timeline"} {
		t.Run(name, func(t *testing.T) {
			s, r := spokenCommandFixture(t, name)
			if r.Validate(s) == nil {
				t.Fatal("fixture no longer demonstrates the real failure")
			}
			r = normalizeGeneratedStructures(r, s)
			if err := r.Validate(s); err != nil {
				t.Fatal(err)
			}
			if name == "table" {
				if r.TableCreations[0].Rows[0].Cells[2].Number != "18" {
					t.Fatal("value changed")
				}
				r.TableCreations[0].Rows[0].Cells[0].Sources[0].SourceID = "invented-source"
			} else {
				if *r.TimelineCreations[0].Events[3].Minute != 900 {
					t.Fatal("time changed")
				}
				r.TimelineCreations[0].Events[0].Period = "afternoon"
			}
			if normalizeGeneratedStructures(r, s).Validate(s) == nil {
				t.Fatal("invalid evidence/conflicting time accepted")
			}
		})
	}
}

func TestReflowExactTextAndInstructionProvenance(t *testing.T) {
	s, r := spokenCommandFixture(t, "unordered")
	r.BlockEdits = nil
	r.Passages = nil
	r.ParagraphCommands = []ParagraphCommand{{ID: uuid.NewString(), Kind: "reflow", BlockIDs: []string{s.Blocks[0].ID}, SourceID: s.PendingUtterances[0].ID, Instruction: s.PendingUtterances[0].Text,
		Fragments: []ParagraphFragment{{"周末出门打算带几样东西：", "body"}, {"水杯、", "unorderedListItem"}, {"雨伞、", "unorderedListItem"}, {"充电宝。", "unorderedListItem"}}}}
	r.SourcePartitions[0].Segments[0].BlockIDs = []string{s.Blocks[0].ID}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	r.ParagraphCommands[0].Fragments[1].Text = "手机、"
	if r.Validate(s) == nil {
		t.Fatal("reflow rewrote source text")
	}
}

func TestReflowRestoresOnlyBoundarySeparators(t *testing.T) {
	parts := []ParagraphFragment{{"准备：", "body"}, {"水杯", "unorderedListItem"}, {"雨伞", "unorderedListItem"}}
	text := "准备：水杯、雨伞。"
	got := restoreReflowSeparators(text, parts)
	if got[1].Text != "水杯、" || got[2].Text != "雨伞。" {
		t.Fatal(got)
	}
	for _, bad := range []string{"准备：水杯、手机、雨伞。", "准备：雨伞、水杯。", "准备：水杯、不是雨伞。"} {
		result := restoreReflowSeparators(bad, parts)
		joined := ""
		for _, p := range result {
			joined += p.Text
		}
		if joined == bad {
			t.Fatal("silently repaired missing/reordered content")
		}
	}
}

func TestReflowNeverCreatesPunctuationOnlyParagraphs(t *testing.T) {
	parts := []ParagraphFragment{{"准备：", "body"}, {"水杯", "unorderedListItem"}, {"、", "body"}, {"雨伞。", "unorderedListItem"}}
	got := coalesceReflowSeparators(parts)
	if len(got) != 3 || got[1].Text != "水杯、" || got[1].Style != "unorderedListItem" {
		t.Fatal(got)
	}
}
