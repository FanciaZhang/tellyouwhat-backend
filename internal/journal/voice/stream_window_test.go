package voice

import (
	"errors"
	"strings"
	"testing"
)

func TestRollingStreamWindowPreservesEarlierSpeakersAndReplacesHypotheses(t *testing.T) {
	var w streamUtteranceWindow
	first := StreamUtterance{Text: "我们约十点。", StartMilliseconds: 0, EndMilliseconds: 1000, Definite: true, Speaker: "1"}
	wrong := StreamUtterance{Text: "十一点", StartMilliseconds: 1200, EndMilliseconds: 1800, Speaker: "1"}
	w.merge(Transcript{Text: first.Text + wrong.Text, Utterances: []StreamUtterance{first, wrong}})
	correction := StreamUtterance{Text: "十点半。", StartMilliseconds: 1200, EndMilliseconds: 2000, Definite: true, Speaker: "2", AcousticEmotion: "surprised"}
	got, err := w.merge(Transcript{Text: first.Text + correction.Text, Utterances: []StreamUtterance{correction}})
	if err != nil || len(got.Utterances) != 2 || got.Utterances[0].Speaker != "1" || got.Utterances[1].Text != correction.Text || got.Utterances[1].Speaker != "2" {
		t.Fatalf("lost or duplicated a turn: %+v", got)
	}
	final, err := w.merge(Transcript{Text: got.Text, Final: true})
	if err != nil || final.Text != got.Text || len(final.Utterances) != 2 {
		t.Fatal("empty final metadata erased evidence")
	}
	var next streamUtteranceWindow
	if result, err := next.merge(Transcript{Text: "新的连接"}); err != nil || len(result.Utterances) != 0 {
		t.Fatal("speaker evidence crossed connections")
	}
}

func TestRollingStreamWindowAcceptsFullCorrectionAndBoundsEvidence(t *testing.T) {
	var w streamUtteranceWindow
	w.merge(Transcript{Utterances: []StreamUtterance{{Text: "旧句", StartMilliseconds: 0, EndMilliseconds: 1000, Definite: true}}})
	got, err := w.merge(Transcript{Text: "完整改句", Utterances: []StreamUtterance{{Text: "完整改句", StartMilliseconds: 0, EndMilliseconds: 1100, Definite: true, Speaker: "2"}}})
	if err != nil || len(got.Utterances) != 1 || got.Utterances[0].Text != got.Text {
		t.Fatal("full correction duplicated older text")
	}
	oversized := make([]StreamUtterance, maxRecognitionUtterances+1)
	for i := range oversized {
		oversized[i].Text = "句"
	}
	if _, err = w.merge(Transcript{Text: "仍保留原文", Utterances: oversized}); !errors.Is(err, errRecognitionContext) {
		t.Fatal("excess evidence must fail explicitly, not flatten source attribution")
	}
	got, err = w.merge(Transcript{Text: "完整改句", Final: true})
	if err != nil || len(got.Utterances) != 1 || got.Utterances[0].Speaker != "2" {
		t.Fatal("failed update erased previously confirmed evidence")
	}
}

func TestConfirmedASRTurnSurvivesEmptyOrOmittedLaterWindow(t *testing.T) {
	for _, emptyPlaceholder := range []bool{true, false} {
		var w streamUtteranceWindow
		first := StreamUtterance{Text: "第一句。", StartMilliseconds: 0, EndMilliseconds: 1000, Definite: true, Speaker: "0"}
		middle := StreamUtterance{Text: "第二句。", StartMilliseconds: 2000, EndMilliseconds: 3000, Definite: true, Speaker: "1"}
		last := StreamUtterance{Text: "第三句。", StartMilliseconds: 4000, EndMilliseconds: 5000, Definite: true, Speaker: "0"}
		w.merge(Transcript{Text: first.Text + middle.Text + last.Text, Utterances: []StreamUtterance{first, middle, last}})
		incoming := []StreamUtterance{first, last}
		if emptyPlaceholder {
			blank := middle
			blank.Text = ""
			incoming = []StreamUtterance{first, blank, last}
		}
		got, err := w.merge(Transcript{Text: first.Text + last.Text, Final: true, Utterances: incoming})
		if err != nil || got.Text != "第一句。第二句。第三句。" || got.ProviderText != "第一句。第三句。" || len(got.Utterances) != 3 || got.Utterances[1].Speaker != "1" {
			t.Fatal("a later empty window erased confirmed speech", got, err)
		}
		middle.Text = "修正的第二句。"
		corrected, err := w.merge(Transcript{Text: first.Text + middle.Text + last.Text, Utterances: []StreamUtterance{first, middle, last}})
		if err != nil || len(corrected.Utterances) != 3 || corrected.Utterances[1].Text != middle.Text {
			t.Fatal("retention blocked a real nonempty correction", corrected, err)
		}
	}
}

func TestEmptyFinalCannotPromoteOrRestoreUnconfirmedHypothesis(t *testing.T) {
	var w streamUtteranceWindow
	u := StreamUtterance{Text: "尚未确认。", StartMilliseconds: 0, EndMilliseconds: 1000}
	w.merge(Transcript{Text: u.Text, Utterances: []StreamUtterance{u}})
	u.Text, u.Definite = "", true
	_, err := w.merge(Transcript{Final: true, Utterances: []StreamUtterance{u}})
	if !errors.Is(err, errEmptyCompletedUtterance) {
		t.Fatal("empty completion must request bounded recovery, not commit a discarded hypothesis", err)
	}
}

func TestRealSecondPassEmptyMiddleTurnRequestsRecoveryWithoutDroppingSource(t *testing.T) {
	// Structure and times from the 138-second synthetic App request. At 48.4s
	// the provider showed the coffee sentence; at 49.4s it completed that same
	// interval with empty text while retaining the preceding six sentences.
	var w streamUtteranceWindow
	first := StreamUtterance{Text: "那里的风有点凉。", StartMilliseconds: 36012, EndMilliseconds: 43021, Definite: true, Speaker: "1"}
	coffee := StreamUtterance{Text: "后来我提议去附近的咖啡店，想尝一尝他们家的拿铁", StartMilliseconds: 43181, EndMilliseconds: 48201}
	if _, err := w.merge(Transcript{Text: first.Text + coffee.Text, Utterances: []StreamUtterance{first, coffee}}); err != nil {
		t.Fatal(err)
	}
	blank := coffee
	blank.Text, blank.EndMilliseconds, blank.Definite = "", 48521, true
	if _, err := w.merge(Transcript{Text: first.Text, Utterances: []StreamUtterance{first, blank}}); !errors.Is(err, errEmptyCompletedUtterance) {
		t.Fatal("blank completion silently consumed audible provisional speech", err)
	}
	if len(w.stable) != 1 || w.stable[0].Text != first.Text || len(w.pending) != 1 || w.pending[0].Text != coffee.Text {
		t.Fatal("failed update mutated prior evidence")
	}
	var retry streamUtteranceWindow
	coffee.Text += "。"
	coffee.Definite, coffee.Speaker = true, "0"
	got, err := retry.merge(Transcript{Text: first.Text + coffee.Text, Final: true, Utterances: []StreamUtterance{first, coffee}})
	if err != nil || len(got.Utterances) != 2 || got.Utterances[1].Text != coffee.Text {
		t.Fatal("real nonempty recognition cannot replace the missing result", err)
	}
	var silence streamUtteranceWindow
	if got, err := silence.merge(Transcript{Final: true, Utterances: []StreamUtterance{blank}}); err != nil || got.Text != "" {
		t.Fatal("genuine empty recognition must remain valid", err)
	}
}

func TestContinuousStreamKeepsHundredsOfTurnsAndTheirSpeakerScopes(t *testing.T) {
	var w streamUtteranceWindow
	for i := 0; i < 600; i++ {
		u := StreamUtterance{Text: "自然句段。", StartMilliseconds: i * 2000, EndMilliseconds: i*2000 + 1500, Definite: true, Speaker: []string{"0", "1"}[i%2]}
		got, err := w.merge(Transcript{Text: strings.Repeat(u.Text, i+1), Utterances: []StreamUtterance{u}})
		if err != nil || len(got.Utterances) != i+1 || got.Utterances[0].StartMilliseconds != 0 || got.Utterances[i].Speaker != u.Speaker {
			t.Fatalf("turn %d lost canonical paragraphs: count=%d err=%v", i, len(got.Utterances), err)
		}
	}
	got, err := w.merge(Transcript{Text: strings.Repeat("自然句段。", 600), Final: true})
	if err != nil || len(got.Utterances) != 600 || got.Utterances[599].EndMilliseconds != 1199500 {
		t.Fatal("long final erased earlier turns", err)
	}
}

func TestSpeechEmptyFinalAfterPartialRequestsRetryWithoutInventingText(t *testing.T) {
	var guard speechResultGuard
	partial := Transcript{Text: "周末想带水杯", Utterances: []StreamUtterance{{Text: "周末想带水杯", Definite: false}}}
	if _, err := guard.accept(partial); err != nil {
		t.Fatal(err)
	}
	if held, err := guard.accept(Transcript{}); err != nil || held.Text != partial.Text || held.Final || held.Utterances[0].Definite {
		t.Fatal("lost or promoted partial", held, err)
	}
	if _, err := guard.accept(Transcript{Final: true}); err == nil {
		t.Fatal("empty final committed over spoken content")
	}
	var silence speechResultGuard
	if final, err := silence.accept(Transcript{Final: true}); err != nil || final.Text != "" {
		t.Fatal("genuine silence rejected")
	}
}
