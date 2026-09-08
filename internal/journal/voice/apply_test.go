package voice

import (
	"errors"
	"testing"
	"unicode/utf8"
)

func TestApplyRevisionPreservesEditOrderWithoutLockingStyle(t *testing.T) {
	id := "30000000-0000-4000-8000-000000000001"
	media := "30000000-0000-4000-8000-000000000002"
	s := Snapshot{WritingStyle: StyleNatural, Revision: 1, Blocks: []Block{{ID: id, Text: "我和小林去了河边。"}, {ID: media}}, Transcript: "我和小明去了河边。", MediaOnlyBlockIDs: []string{media}, ManualEdits: []ManualEdit{{BlockID: id, Before: "小明", After: "小林", PendingEarlierSpeech: true}}}
	r := Revision{BaseRevision: 1, TranscriptRevision: 1, Patches: []Patch{{ID: id, Text: "我和小明去了河边。"}}}
	if _, err := ApplyRevision(s, r); !errors.Is(err, ErrConflict) {
		t.Fatal("earlier speech reversed manual replacement", err)
	}
	r.Patches[0].Text = "午后，我们一起沿着河边走了走。"
	blocks, err := ApplyRevision(s, r)
	if err != nil || len(blocks) != 2 || blocks[1].ID != media {
		t.Fatal("style paraphrase or media order was blocked", err)
	}
	s.ManualEdits[0].PendingEarlierSpeech = false
	s.ManualEdits[0].TranscriptOffset = utf8.RuneCountInString(s.Transcript)
	s.Transcript += "刚才说错了，同行的是小明。"
	r.Patches[0].Text = "我和小明去了河边。"
	if _, err := ApplyRevision(s, r); err != nil {
		t.Fatal("later explicit correction blocked", err)
	}
	s.ManualEdits[0].PendingEarlierSpeech = true
	s.ManualEdits[0].After = ""
	s.Blocks[0].Text = "我去了河边。"
	if _, err := ApplyRevision(s, r); !errors.Is(err, ErrConflict) {
		t.Fatal("earlier speech restored a deletion", err)
	}
}
