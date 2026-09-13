package voice

import "testing"

func TestRollingStreamWindowPreservesEarlierSpeakersAndReplacesHypotheses(t *testing.T) {
	var w streamUtteranceWindow
	first := StreamUtterance{Text: "我们约十点。", StartMilliseconds: 0, EndMilliseconds: 1000, Definite: true, Speaker: "1"}
	wrong := StreamUtterance{Text: "十一点", StartMilliseconds: 1200, EndMilliseconds: 1800, Speaker: "1"}
	w.merge(Transcript{Text: first.Text + wrong.Text, Utterances: []StreamUtterance{first, wrong}})
	correction := StreamUtterance{Text: "十点半。", StartMilliseconds: 1200, EndMilliseconds: 2000, Definite: true, Speaker: "2", AcousticEmotion: "surprised"}
	got := w.merge(Transcript{Text: first.Text + correction.Text, Utterances: []StreamUtterance{correction}})
	if len(got.Utterances) != 2 || got.Utterances[0].Speaker != "1" || got.Utterances[1].Text != correction.Text || got.Utterances[1].Speaker != "2" {
		t.Fatalf("lost or duplicated a turn: %+v", got)
	}
	final := w.merge(Transcript{Text: got.Text, Final: true})
	if final.Text != got.Text || len(final.Utterances) != 2 {
		t.Fatal("empty final metadata erased evidence")
	}
	var next streamUtteranceWindow
	if result := next.merge(Transcript{Text: "新的连接"}); len(result.Utterances) != 0 {
		t.Fatal("speaker evidence crossed connections")
	}
}

func TestRollingStreamWindowAcceptsFullCorrectionAndBoundsEvidence(t *testing.T) {
	var w streamUtteranceWindow
	w.merge(Transcript{Utterances: []StreamUtterance{{Text: "旧句", StartMilliseconds: 0, EndMilliseconds: 1000, Definite: true}}})
	got := w.merge(Transcript{Text: "完整改句", Utterances: []StreamUtterance{{Text: "完整改句", StartMilliseconds: 0, EndMilliseconds: 1100, Definite: true, Speaker: "2"}}})
	if len(got.Utterances) != 1 || got.Utterances[0].Text != got.Text {
		t.Fatal("full correction duplicated older text")
	}
	oversized := make([]StreamUtterance, 257)
	got = w.merge(Transcript{Text: "仍保留原文", Utterances: oversized})
	if got.Text != "仍保留原文" || len(got.Utterances) != 0 {
		t.Fatal("unbounded evidence or lost text")
	}
}
