package voice

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"golang.org/x/net/websocket"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func longEditorialFixture() Snapshot {
	block := uuid.NewString()
	old := strings.Repeat("过去的口述。", 600)
	return Snapshot{Revision: 7, Blocks: []Block{{block, strings.Repeat("保留已有文字。", 3000)}}, Transcript: old + "后来回了家。", rewriteAcknowledged: old}
}

func TestEditorialWindowKeepsPendingSpeechAndAbsoluteEditTiming(t *testing.T) {
	s := longEditorialFixture()
	boundary := len([]rune(s.rewriteAcknowledged))
	s.ManualEdits = []ManualEdit{{BlockID: s.Blocks[0].ID, Before: "原名", After: "手改名", TranscriptOffset: boundary}}
	d := editorialDocument(s)
	if d.ContextWindow == nil || d.ContextWindow.TranscriptStart != boundary-rewriteHistoryCharacters {
		t.Fatal("missing acknowledged boundary")
	}
	if len(d.Transcript) != 2 || d.Transcript[1].Start != boundary || d.Transcript[1].Text != "后来回了家。" || !d.ManualEdits[0].HasLaterSpeech {
		t.Fatal("lost absolute order or pending speech")
	}
	s.ManualEdits[0].PendingEarlierSpeech = true
	d = editorialDocument(s)
	if d.ManualEdits[0].HasLaterSpeech || d.ManualEdits[0].TranscriptOffset != len([]rune(s.Transcript)) {
		t.Fatal("late ASR can overturn a later hand edit")
	}
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), s.rewriteAcknowledged) {
		t.Fatal("duplicated full old speech")
	}
	for _, unprocessed := range []Snapshot{
		{Revision: s.Revision, Blocks: s.Blocks, Transcript: s.Transcript},
		{Revision: s.Revision, Blocks: s.Blocks, Transcript: s.Transcript + strings.Repeat("新", 4001), rewriteAcknowledged: s.rewriteAcknowledged},
	} {
		if editorialDocument(unprocessed).ContextWindow != nil {
			t.Fatal("unprocessed source was cropped")
		}
	}
	// A revised interim result is pending again from the first changed rune.
	s = longEditorialFixture()
	s.Transcript = "改了开头。" + s.Transcript
	if incrementalTranscriptStart(s) != 0 {
		t.Fatal("revised source was treated as acknowledged")
	}
}

func TestEditorialWindowSplicesOnlyVisibleRangesAndPreservesLongBlocks(t *testing.T) {
	s := longEditorialFixture()
	d := editorialDocument(s)
	used := 0
	for _, b := range d.Blocks {
		used += len([]rune(b.Text))
	}
	if used > rewriteBodyCharacters || len(d.Blocks) > 8 || d.ContextWindow.OmittedBodyCharacters == 0 {
		t.Fatal("unbounded document")
	}
	first, last := d.ContextWindow.Fragments[0], d.ContextWindow.Fragments[len(d.ContextWindow.Fragments)-1]
	insertion := uuid.NewString()
	r := Revision{BaseRevision: 7, TranscriptRevision: 4, Patches: []Patch{
		{ID: first.ID, Text: "改了开头。"}, {ID: last.ID, Text: "修改结尾。"},
		{ID: insertion, Text: "新增内容。", AfterID: last.ID}, {ID: uuid.NewString(), Text: "继续补充。", AfterID: insertion},
	}}
	got, err := expandEditorialRevision(r, d, s)
	if err != nil {
		t.Fatal(err)
	}
	original := []rune(s.Blocks[0].Text)
	want := "改了开头。" + string(original[first.End:last.Start]) + "修改结尾。\n\n新增内容。\n\n继续补充。"
	if len(got.Patches) != 1 || got.Patches[0].ID != s.Blocks[0].ID || got.Patches[0].Text != want || got.Patches[0].AfterID != "" {
		t.Fatal("hidden text or persistent identity changed")
	}
	// A model cannot overwrite a hidden real block using a known original ID.
	r.Patches = []Patch{{ID: s.Blocks[0].ID, Text: "全删了"}}
	if _, err := expandEditorialRevision(r, d, s); err == nil {
		t.Fatal("accepted a patch outside the provided scope")
	}
}

func TestEditorialWindowRetrievesOlderCorrectionTarget(t *testing.T) {
	s := longEditorialFixture()
	s.Blocks = nil
	for i := 0; i < 20; i++ {
		text := strings.Repeat("平常的经过。", 150)
		if i == 7 {
			text = "那天的紫藤花架在北门。" + text
		}
		s.Blocks = append(s.Blocks, Block{uuid.NewString(), text})
	}
	s.Transcript = s.rewriteAcknowledged + "补充一下，紫藤花架是在南门，不是北门。"
	d := editorialDocument(s)
	found := false
	for _, b := range d.Blocks {
		found = found || strings.Contains(b.Text, "紫藤花架在北门")
	}
	if !found {
		t.Fatal("a distant matching target was lost")
	}
}

func TestEditorialWindowNeverTrustsClientCheckpoint(t *testing.T) {
	var s Snapshot
	if err := json.Unmarshal([]byte(`{"transcript":"尚未整理", "rewriteAcknowledged":"尚未整理"}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.rewriteAcknowledged != "" || incrementalTranscriptStart(s) != 0 {
		t.Fatal("client can forge processed speech")
	}
	b := Block{uuid.NewString(), "用户在等待时修改了"}
	if acceptsEditorialPatches([]Block{b}, []Patch{{ID: b.ID, Text: "迟到结果"}}) {
		t.Fatal("unapplied patch advanced boundary")
	}
	if !acceptsEditorialPatches([]Block{b}, []Patch{{ID: b.ID, Text: b.Text}}) {
		t.Fatal("matching acknowledgement rejected")
	}
}

func TestCompletedRecordingAlwaysRetainsWholeSource(t *testing.T) {
	s := longEditorialFixture()
	r := recordingContextFixture(t)
	s.RecordingContext = &r
	s.Transcript = r.Analysis.Text
	s.rewriteAcknowledged = s.Transcript
	d := editorialDocument(s)
	if d.ContextWindow != nil || len(d.Blocks) != len(s.Blocks) || d.Blocks[0].Text != s.Blocks[0].Text {
		t.Fatal("full review was cropped")
	}
}

func TestRecordingEditorialDoesNotRepeatAnonymousTranscript(t *testing.T) {
	r := recordingContextFixture(t)
	s := Snapshot{Revision: 1, Blocks: []Block{{uuid.NewString(), "可恢复草稿"}}, Transcript: r.Analysis.Text, RecordingContext: &r}
	d := editorialDocument(s)
	if len(d.Transcript) != 0 || len(d.RecordingContext.Utterances) != len(r.Analysis.Utterances) {
		t.Fatal("dropped source or repeated an anonymous transcript")
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, exists := wire["transcript"]; exists {
		t.Fatal("embedded Snapshot leaked the anonymous transcript")
	}
	for i, u := range r.Analysis.Utterances {
		if d.RecordingContext.Utterances[i].Text != u.Text || d.RecordingContext.Utterances[i].SourceID != u.ID {
			t.Fatal("source altered")
		}
	}
}

type checkpointRewriter struct {
	calls      chan Snapshot
	unresolved bool
}

func (m checkpointRewriter) Rewrite(_ context.Context, s Snapshot, tr int) (RewriteResult, error) {
	m.calls <- s
	var questions []string
	if m.unresolved {
		questions = []string{"前面的发言是谁说的？"}
	}
	return RewriteResult{Revision: Revision{BaseRevision: s.Revision, TranscriptRevision: tr, Patches: []Patch{{ID: s.Blocks[0].ID, Text: s.Blocks[0].Text + "整理。"}}, Questions: questions}}, nil
}
func TestSocketAdvancesWindowOnlyAfterAppliedRevision(t *testing.T) {
	for _, tc := range []struct{ accept, unresolved bool }{{false, false}, {true, false}, {true, true}} {
		accept := tc.accept
		t.Run(strconv.FormatBool(accept)+"/question="+strconv.FormatBool(tc.unresolved), func(t *testing.T) {
			conn := &scriptedConnection{make(chan Transcript, 4), make(chan struct{})}
			calls := make(chan Snapshot, 4)
			service := &Service{Store: NewMemoryStore(), Speech: streamingSpeech{conn}, Model: checkpointRewriter{calls, tc.unresolved}, Secret: make([]byte, 32)}
			session := uuid.NewString()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { service.Serve(w, r, session) }))
			defer server.Close()
			ticket, err := service.Issue(context.Background(), Identity{Owner: "checkpoint", Anchor: time.Now().AddDate(0, -1, 0), ExpiresAt: time.Now().Add(time.Hour)}, session)
			if err != nil {
				t.Fatal(err)
			}
			config, _ := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http"), "http://localhost")
			config.Header.Set("Authorization", "Bearer "+ticket.Token)
			ws, err := websocket.DialConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			ws.SetDeadline(time.Now().Add(5 * time.Second))
			var event Event
			receive := func(kind string) {
				t.Helper()
				if err := websocket.JSON.Receive(ws, &event); err != nil || event.Type != kind {
					t.Fatalf("expected %s: %+v %v", kind, event, err)
				}
			}
			receive("ready")
			s := longEditorialFixture()
			s.rewriteAcknowledged = ""
			websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &s})
			receive("revision")
			first := <-calls
			if first.rewriteAcknowledged != "" {
				t.Fatal("first source already acknowledged")
			}
			if accept {
				s.Blocks[0].Text = event.Revision.Patches[0].Text
			} else {
				s.Blocks[0].Text = "用户另改的正文。"
			}
			s.Revision++
			websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &s})
			websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: uuid.NewString(), PCM: make([]byte, 6400)})
			conn.result <- Transcript{Text: "补充了新的内容。", Stable: "补充了新的内容。"}
			receive("transcript")
			receive("revision")
			second := <-calls
			if accept && !tc.unresolved && (second.rewriteAcknowledged != s.Transcript || incrementalTranscriptStart(second) == 0) {
				t.Fatal("applied result did not advance source")
			}
			if (!accept || tc.unresolved) && second.rewriteAcknowledged != "" {
				t.Fatal("unapplied revision cropped source")
			}
		})
	}
}

func TestArkWindowRequestAndExpansionKeepHiddenTextAndUsage(t *testing.T) {
	s := longEditorialFixture()
	var sentBytes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire struct{ Input, Instructions string }
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		sentBytes = len(wire.Input)
		var input struct{ Document rewriteDocument }
		if err := json.Unmarshal([]byte(wire.Input), &input); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		d := input.Document
		if d.ContextWindow == nil || strings.Contains(wire.Input, s.Blocks[0].Text) || !strings.Contains(wire.Instructions, incrementalWindowInstructions) {
			t.Error("window was not used on the actual model wire")
		}
		last := d.Blocks[len(d.Blocks)-1]
		revision := Revision{BaseRevision: s.Revision, TranscriptRevision: 4, Patches: []Patch{{ID: last.ID, Text: last.Text + "新事实。"}}}
		encoded, _ := json.Marshal(revision)
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 100, "output_tokens": 20}, "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(encoded)}}}}})
	}))
	defer server.Close()
	got, err := (ArkRewriter{BaseURL: server.URL, APIKey: "test", Model: "test", HTTP: server.Client()}).Rewrite(context.Background(), s, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Revision.Patches) != 1 || got.Revision.Patches[0].Text != s.Blocks[0].Text+"新事实。" || got.Revision.Patches[0].ID != s.Blocks[0].ID || got.InputTokens != 100 || got.OutputTokens != 20 {
		t.Fatal("window expansion lost text, identity or real token usage")
	}
	full, _ := json.Marshal(s)
	if sentBytes >= len(full)/2 {
		t.Fatalf("large incremental input not meaningfully reduced: %d >= %d/2", sentBytes, len(full))
	}
	t.Logf("synthetic long-document payload: full snapshot %d bytes; bounded model data %d bytes", len(full), sentBytes)
}

func TestEditorialWindowDoesNotSplitUnbrokenGraphemes(t *testing.T) {
	s := longEditorialFixture()
	s.Blocks[0].Text = strings.Repeat("👨‍👩‍👧", 2000)
	d := editorialDocument(s)
	if len(d.ContextWindow.Fragments) != 0 || d.Blocks[0].Text != s.Blocks[0].Text {
		t.Fatal("split an unbroken grapheme sequence")
	}
}
