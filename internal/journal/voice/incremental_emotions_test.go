package voice

import "testing"

func TestUnsupportedEmotionDoesNotDiscardManuscript(t *testing.T) {
	r := Revision{BlockEdits: []BlockEdit{{Text: "心情很好"}}, Emotions: []EmotionPlacement{{SourceID: "text-only", Kind: "happy"}, {SourceID: "acoustic", Kind: "calm"}}, OverallEmotion: "happy"}
	s := Snapshot{PendingUtterances: []SourceUtterance{{ID: "text-only", Text: "心情很好"}}}
	got := groundedIncrementalEmotions(r, s)
	if len(got.Emotions) != 0 || got.OverallEmotion != "" || got.BlockEdits[0].Text != r.BlockEdits[0].Text {
		t.Fatal("unsupported decoration retained or prose changed")
	}
	s.PendingUtterances = append(s.PendingUtterances, SourceUtterance{ID: "acoustic", AcousticEmotion: "neutral"})
	got = groundedIncrementalEmotions(r, s)
	if len(got.Emotions) != 1 || got.Emotions[0].SourceID != "acoustic" {
		t.Fatal("real acoustic evidence lost")
	}
}
