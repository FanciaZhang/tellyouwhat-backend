package voice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

func polishFixture() Snapshot {
	id := uuid.NewString()
	return Snapshot{DictationMode: true, WritingStyle: StyleDocumentary, Blocks: []Block{{ID: id, Text: "今天，嗯，去了河边。", Style: "body"}}, Polish: &PolishRequest{Targets: []PolishTarget{{Style: "body", ID: id, Text: "今天，嗯，去了河边。", SourceText: "今天，嗯，去了河边。", SourceIDs: []string{uuid.NewString()}}}, Context: []Block{}}}
}

func TestBlankPolishTargetNeedsActualSourceAndCannotBeSpacingOnlyTask(t *testing.T) {
	s := polishFixture()
	target := &s.Polish.Targets[0]
	target.Text = ""
	target.Turns = []SourceUtterance{{ID: target.SourceIDs[0], Text: target.SourceText}}
	if err := s.Validate(); err != nil {
		t.Fatal("an empty presentation must still allow grounded source recovery", err)
	}
	target.SourceText = " "
	if err := s.Validate(); err == nil {
		t.Fatal("a whitespace-only draft cannot become an ordinary editing task")
	}
	target.SourceText = target.Turns[0].Text
	target.Turns = nil
	if err := s.Validate(); err == nil {
		t.Fatal("empty presentation without source evidence accepted")
	}
}

func TestIdentityOperationSpansAreExcludedFromModelButRetainedInEvidence(t *testing.T) {
	s := polishFixture()
	target := &s.Polish.Targets[0]
	command, story := "把我标成小林。", "今天在河边散步很舒服。"
	target.Text = story
	target.SourceText = command + story
	target.Turns = []SourceUtterance{{ID: target.SourceIDs[0], Text: target.SourceText, ExcludedText: []string{command}}}
	prepared, err := PrepareRewrite(context.Background(), s, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.Input, command) || !strings.Contains(body.Input, story) {
		t.Fatal("operation leaked into prose input")
	}
	output := `{"paragraphs":[{"targetIDs":["` + target.ID + `"],"style":"body","text":"今天在河边散步很舒服。"}],"questions":[]}`
	result, err := decodePolish(output, *s.Polish)
	if err != nil {
		t.Fatal("story without command rejected", err)
	}
	if !result.Targets[0].Turns[0].Equal(target.Turns[0]) {
		t.Fatal("raw evidence was altered")
	}
	changed := target.Turns[0]
	changed.ExcludedText = nil
	if changed.Equal(target.Turns[0]) {
		t.Fatal("command edit must invalidate stale acknowledgement")
	}
	target.Turns[0].ExcludedText = []string{"未说过的操作"}
	if s.Polish.Validate() == nil {
		t.Fatal("unbacked exclusion accepted")
	}
	target.Turns[0].ExcludedText = []string{command, command}
	if s.Polish.Validate() == nil {
		t.Fatal("duplicate operation accepted")
	}
}
func TestPolishRequestUsesSmallSchemaAndEffectiveBudgetLimits(t *testing.T) {
	s := polishFixture()
	prepared, err := PrepareRewrite(context.Background(), s, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prepared.Body), "diagramCreations") || strings.Contains(string(prepared.Body), "semanticState") || len(prepared.Body) > 8500 {
		t.Fatalf("ordinary task has structural context or exceeds 8500 bytes: %d bytes", len(prepared.Body))
	}
	if body["max_output_tokens"].(float64) > 2048 || prepared.TimeoutSeconds > 25 || prepared.Parameters.ReasoningEffort != "disabled" {
		t.Fatal("unbounded polish parameters")
	}
}
func TestPolishBaselineIsServerOwnedAndUnknownParagraphsAreRejected(t *testing.T) {
	s := polishFixture()
	target := s.Polish.Targets[0]
	output := `{"paragraphs":[{"targetIDs":["` + target.ID + `"],"style":"body","text":"今天去了河边。"}],"questions":[]}`
	result, err := decodePolish(output, *s.Polish)
	if err != nil {
		t.Fatal(err)
	}
	if result.Targets[0].Text != target.Text || result.Targets[0].SourceText != target.SourceText {
		t.Fatal("lost immutable baseline")
	}
	if _, err = decodePolish(strings.Replace(output, target.ID, uuid.NewString(), 1), *s.Polish); err == nil {
		t.Fatal("accepted unrelated paragraph")
	}
	if _, err = decodePolish(output+" {}", *s.Polish); err == nil {
		t.Fatal("accepted multiple outputs")
	}
}

func TestNarrativeAcceptsMergedDialogueAndPreservesTrustedSpeakerEvidence(t *testing.T) {
	s := polishFixture()
	first := &s.Polish.Targets[0]
	first.Turns = []SourceUtterance{{ID: first.SourceIDs[0], Text: first.SourceText, Speaker: "segment:1", Person: "我"}}
	second := PolishTarget{Style: "body", ID: uuid.NewString(), Text: "啊，我更想先去图书馆还书。", SourceText: "啊，我更想先去图书馆还书。", SourceIDs: []string{uuid.NewString()}}
	second.Turns = []SourceUtterance{{ID: second.SourceIDs[0], Text: second.SourceText, Speaker: "segment:2", Person: "妻子"}}
	s.Polish.Targets = append(s.Polish.Targets, second)
	if err := s.Polish.Validate(); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareRewrite(context.Background(), s, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input string `json:"input"`
	}
	if err = json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	var input struct {
		Targets []PolishTarget `json:"targets"`
	}
	if err = json.Unmarshal([]byte(body.Input), &input); err != nil {
		t.Fatal(err)
	}
	if input.Targets[1].Turns[0].Person != "妻子" || input.Targets[0].Turns[0].Speaker == input.Targets[1].Turns[0].Speaker {
		t.Fatal("narrative lost trusted actors")
	}
	output := `{"paragraphs":[{"targetIDs":["` + s.Polish.Targets[0].ID + `","` + second.ID + `"],"style":"body","text":"我想去河边，妻子更想先去图书馆还书。"}],"questions":[]}`
	result, err := decodePolish(output, *s.Polish)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Paragraphs) != 1 || len(result.Paragraphs[0].TargetIDs) != 2 || result.Targets[1].Turns[0].Person != "妻子" {
		t.Fatal("failed to merge dialogue with immutable source evidence")
	}
}

func TestNarrativeRejectsMissingSubstantiveSourceButCanRemoveOnlyFillers(t *testing.T) {
	p := *polishFixture().Polish
	if _, err := decodePolish(`{"paragraphs":[],"questions":[]}`, p); err == nil {
		t.Fatal("substantive speech disappeared")
	}
	second := p.Targets[0]
	second.ID = uuid.NewString()
	p.Targets = append(p.Targets, second)
	if _, err := decodePolish(`{"paragraphs":[{"targetIDs":["`+p.Targets[0].ID+`"],"style":"body","text":"去了河边。"}],"questions":[]}`, p); err == nil {
		t.Fatal("partial source coverage accepted")
	}
	for i := range p.Targets {
		p.Targets[i].SourceText = "嗯，啊，呃。"
	}
	if result, err := decodePolish(`{"paragraphs":[],"questions":[]}`, p); err != nil || len(result.Paragraphs) != 0 {
		t.Fatal("cannot remove a batch of pure fillers", err)
	}
}

func TestNarrativeNormalizesRealParagraphBreaksWithSharedSources(t *testing.T) {
	p := *polishFixture().Polish
	result, err := decodePolish(`{"paragraphs":[{"targetIDs":["`+p.Targets[0].ID+`"],"style":"body","text":"今天去了河边。\n\n在那里散步。"}],"questions":[]}`, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Paragraphs) != 2 || result.Paragraphs[1].Text != "在那里散步。" || result.Paragraphs[1].TargetIDs[0] != p.Targets[0].ID {
		t.Fatal("normalized paragraphs lost source coverage")
	}
}

func TestNarrativeAcknowledgementRequiresTheExactServerResult(t *testing.T) {
	id := uuid.NewString()
	if polishAcknowledged("", id) || polishAcknowledged(uuid.NewString(), id) || polishAcknowledged("", "") {
		t.Fatal("unhandled or different results were acknowledged")
	}
	if !polishAcknowledged(id, id) {
		t.Fatal("unchanged prose must still acknowledge a handled result")
	}
}

func TestContinuousNarrativePreservesRetainedProseWithFillerOnlyNewSpeech(t *testing.T) {
	p := *polishFixture().Polish
	p.Targets[0].RetainedText = "今天去了河边散步。"
	p.Targets[0].SourceText = "嗯，啊，呃。"
	p.Targets[0].Text = p.Targets[0].RetainedText + p.Targets[0].SourceText
	if _, err := decodePolish(`{"paragraphs":[],"questions":[]}`, p); err == nil {
		t.Fatal("filler removal deleted previously organized prose")
	}
	output := `{"paragraphs":[{"targetIDs":["` + p.Targets[0].ID + `"],"style":"body","text":"今天去了河边散步。"}],"questions":[]}`
	if _, err := decodePolish(output, p); err != nil {
		t.Fatal(err)
	}
}

func TestContinuousNarrativeRetainsPrefixAndStableSourceKeysAcrossModelRoundTrip(t *testing.T) {
	s := polishFixture()
	target := &s.Polish.Targets[0]
	target.RetainedText = "上午和妻子去了公园。"
	target.Text = target.RetainedText + target.SourceText
	target.Turns = []SourceUtterance{{ID: target.SourceIDs[0], Text: target.SourceText, Speaker: "segment:1"}}
	target.SourceKeys = []string{uuid.NewString()}
	prepared, err := PrepareRewrite(context.Background(), s, 1, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input string `json:"input"`
	}
	if err = json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	var input struct {
		Targets []PolishTarget `json:"targets"`
	}
	if err = json.Unmarshal([]byte(body.Input), &input); err != nil {
		t.Fatal(err)
	}
	if input.Targets[0].RetainedText != target.RetainedText || len(input.Targets[0].SourceKeys) != 0 {
		t.Fatal("lost paragraph prefix or sent bookkeeping IDs to the model")
	}
	result, err := decodePolish(`{"paragraphs":[{"targetIDs":["`+target.ID+`"],"style":"body","text":"上午和妻子去了公园，随后去河边。"}],"questions":[]}`, *s.Polish)
	if err != nil {
		t.Fatal(err)
	}
	if result.Targets[0].SourceKeys[0] != target.SourceKeys[0] {
		t.Fatal("response cannot match in-flight source")
	}
	s.Polish.Targets[0].SourceKeys = []string{uuid.NewString(), uuid.NewString()}
	if err = s.Polish.Validate(); err == nil {
		t.Fatal("accepted mismatched source keys")
	}
}

type gatedPolishRewriter struct {
	started chan Snapshot
	release chan struct{}
	fail    bool
}

func (m *gatedPolishRewriter) Rewrite(ctx context.Context, s Snapshot, _ int) (RewriteResult, error) {
	select {
	case m.started <- s:
	case <-ctx.Done():
		return RewriteResult{}, ctx.Err()
	}
	select {
	case <-m.release:
	case <-ctx.Done():
		return RewriteResult{}, ctx.Err()
	}
	if m.fail {
		return RewriteResult{}, errors.New("provider unavailable")
	}
	target := s.Polish.Targets[0]
	return RewriteResult{Polish: &PolishRevision{Targets: s.Polish.Targets, Paragraphs: []PolishParagraph{{Style: "body", TargetIDs: []string{target.ID}, Text: "今天去了河边。"}}, Questions: []string{}}}, nil
}
func polishSocket(t *testing.T, model Rewriter, speech Speech) *websocket.Conn {
	t.Helper()
	service := &Service{Store: NewMemoryStore(), Model: model, Speech: speech, Secret: make([]byte, 32)}
	session := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { service.Serve(w, r, session) }))
	t.Cleanup(server.Close)
	ticket, err := service.Issue(context.Background(), Identity{Owner: uuid.NewString(), Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, session)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http"), "http://localhost")
	config.Header.Set("Authorization", "Bearer "+ticket.Token)
	ws, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	ws.SetDeadline(time.Now().Add(8 * time.Second))
	var ready Event
	if err := receiveVoiceResult(ws, &ready); err != nil || ready.Type != "ready" {
		t.Fatalf("ready: %+v %v", ready, err)
	}
	return ws
}
func TestNewDictationBodyDoesNotDiscardInFlightPolish(t *testing.T) {
	model := &gatedPolishRewriter{started: make(chan Snapshot, 2), release: make(chan struct{})}
	ws := polishSocket(t, model, nil)
	snapshot := polishFixture()
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	<-model.started
	// Add a paragraph, advance the document version and retain the first task.
	next := polishFixture()
	next.Revision = 10
	snapshot.Blocks = append(snapshot.Blocks, next.Blocks...)
	snapshot.Polish.Targets = append(snapshot.Polish.Targets, next.Polish.Targets...)
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	close(model.release)
	var event Event
	if err := receiveVoiceResult(ws, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "polish" || len(event.Polish.Targets) != 1 {
		t.Fatalf("lost completed prose: %+v", event)
	}
	// Remove just the processed baseline. The next sentence becomes its own job.
	snapshot.AcknowledgedPolishID = event.Polish.ID
	snapshot.Polish.Targets = snapshot.Polish.Targets[1:]
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	select {
	case started := <-model.started:
		if started.Polish.Targets[0].ID != next.Polish.Targets[0].ID {
			t.Fatal("repeated old paragraph")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queue did not advance after acknowledgement")
	}
}
func TestPolishFailureKeepsAudioReceiptsAndAllowsFinishingAfterPause(t *testing.T) {
	model := &gatedPolishRewriter{started: make(chan Snapshot, 2), release: make(chan struct{}), fail: true}
	close(model.release)
	speech := &scriptedSpeech{}
	ws := polishSocket(t, model, speech)
	snapshot := polishFixture()
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	var event Event
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "rewrite_error" {
		t.Fatalf("polish failure: %+v %v", event, err)
	}
	snapshot.Polish.Paused = true
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	receipt, completed := voiceLifecycleShort(t, ws, uuid.NewString(), uuid.NewString(), make([]byte, 6400))
	if receipt.Milliseconds != 200 || completed.Text == "" || speech.opens.Load() != 1 {
		t.Fatal("audio/ASR acknowledgement lost")
	}
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "finished" {
		t.Fatalf("paused finalization: %+v %v", event, err)
	}
}

func TestHandledPolishCanAdvanceWithUnchangedPendingTargets(t *testing.T) {
	model := &gatedPolishRewriter{started: make(chan Snapshot, 2), release: make(chan struct{})}
	close(model.release)
	ws := polishSocket(t, model, nil)
	snapshot := polishFixture()
	if err := websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot}); err != nil {
		t.Fatal(err)
	}
	<-model.started
	var event Event
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "polish" || event.Polish == nil || uuid.Validate(event.Polish.ID) != nil {
		t.Fatalf("identified polish result: %+v %v", event, err)
	}
	// The App can handle a response without changing text, or skip a stale
	// atomic group while retaining an unchanged source for a fresh attempt.
	snapshot.AcknowledgedPolishID = event.Polish.ID
	if err := websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot}); err != nil {
		t.Fatal(err)
	}
	select {
	case started := <-model.started:
		if !slices.Equal(started.Blocks, snapshot.Blocks) || started.Polish.Targets[0].ID != snapshot.Polish.Targets[0].ID {
			t.Fatal("acknowledgement required a synthetic body mutation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handled unchanged result blocked remaining prose work")
	}
}

func TestCaptureClosedCoalescesRemainingSnapshotsUntilTranscriptionFinishes(t *testing.T) {
	model := &gatedPolishRewriter{started: make(chan Snapshot, 2), release: make(chan struct{})}
	ws := polishSocket(t, model, nil)
	snapshot := polishFixture()
	websocket.JSON.Send(ws, Frame{Type: "capture_closed"})
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	snapshot.Polish.Targets[0].SourceText += "晚上开始读书。"
	snapshot.Polish.Targets[0].Text = snapshot.Polish.Targets[0].SourceText
	snapshot.Blocks[0].Text = snapshot.Polish.Targets[0].Text
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "ping"})
	var event Event
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Type != "pong" {
		t.Fatalf("tail barrier: %+v %v", event, err)
	}
	select {
	case <-model.started:
		t.Fatal("started a fragmented tail before transcription finished")
	default:
	}
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Type != "processing" || event.Stage != "organizing" {
		t.Fatalf("processing: %+v %v", event, err)
	}
	selected := <-model.started
	if selected.Polish.Targets[0].SourceText != snapshot.Polish.Targets[0].SourceText {
		t.Fatal("latest tail was not included")
	}
	close(model.release)
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "polish" {
		t.Fatalf("polish: %+v %v", event, err)
	}
	snapshot.AcknowledgedPolishID = event.Polish.ID
	snapshot.Polish.Targets = nil
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "finished" {
		t.Fatalf("finish: %+v %v", event, err)
	}
	select {
	case <-model.started:
		t.Fatal("duplicate final rewrite")
	default:
	}
}

func TestPausingOrdinaryPolishCancelsModelWithoutFailingTheVoiceSession(t *testing.T) {
	model := &gatedPolishRewriter{started: make(chan Snapshot, 2), release: make(chan struct{})}
	ws := polishSocket(t, model, nil)
	snapshot := polishFixture()
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	var event Event
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Stage != "organizing" {
		t.Fatalf("start: %+v %v", event, err)
	}
	<-model.started
	snapshot.Polish.Paused = true
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Stage != "idle" {
		t.Fatalf("cancel: %+v %v", event, err)
	}
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "finished" {
		t.Fatalf("finish: %+v %v", event, err)
	}
}

func TestPolishRejectsAnEntireMissingTurnEvenWhenTargetIDIsCovered(t *testing.T) {
	p := *polishFixture().Polish
	p.Targets[0].Turns = []SourceUtterance{{ID: uuid.NewString(), Text: "今天商量周末去公园散步。"}, {ID: uuid.NewString(), Text: "晚上读朋友推荐的新书，喜欢勇敢面对困难的故事。"}}
	body := func(text string) string {
		raw, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: text, Style: "body", TargetIDs: []string{p.Targets[0].ID}}}, "questions": []string{}})
		return string(raw)
	}
	if _, err := decodePolish(body("今天商量周末去公园散步。"), p); !errors.Is(err, errPolishMissingSource) {
		t.Fatal("missing second topic accepted", err)
	}
	if _, err := decodePolish(body("今天商量周末去公园散步。晚上读了一本朋友推荐的新书，很喜欢这个勇敢面对困难的故事。"), p); err != nil {
		t.Fatal(err)
	}
}

func TestPolishAnchorsAllowDeduplicationParaphraseAndShortTurns(t *testing.T) {
	for _, test := range []struct {
		sources []string
		output  string
	}{
		{[]string{"嗯，我们商量周末去公园散步。", "我们商量周末去公园散步。"}, "周末我们一起去公园散步。"},
		{[]string{"好。"}, "好。"},
		{[]string{"去东湖。"}, "在东湖散步。"},
		{[]string{"我想10点去公园散步，妻子先到图书馆还书。", "我想十点去公园散步，妻子先到图书馆还书。"}, "我想十点去公园散步，妻子先到图书馆还书。"},
	} {
		sources := []SourceUtterance{}
		for _, text := range test.sources {
			sources = append(sources, SourceUtterance{ID: uuid.NewString(), Text: text})
		}
		if !polishRetainsSourceAnchors(sources, []PolishParagraph{{Text: test.output}}) {
			t.Fatal("faithful rewrite or deduplication rejected", test)
		}
	}
	if polishRetainsSourceAnchors([]SourceUtterance{{Text: "好。"}}, []PolishParagraph{{Text: "。"}}) {
		t.Fatal("punctuation accepted as substantive grounding")
	}
}

func TestPolishSourceAnchorsIncludeStableReadOnlyContextWithoutRepeatingIt(t *testing.T) {
	p := *polishFixture().Polish
	p.Context = []Block{{ID: uuid.NewString(), Style: "body", Text: "今天商量周末去公园散步。"}}
	p.Targets[0].Turns = []SourceUtterance{{ID: uuid.NewString(), Text: "今天商量周末去公园散步。"}, {ID: uuid.NewString(), Text: "晚上读朋友推荐的新书，喜欢勇敢面对困难的故事。"}}
	body := func(text string) string {
		raw, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: text, Style: "body", TargetIDs: []string{p.Targets[0].ID}}}, "questions": []string{}})
		return string(raw)
	}
	if _, err := decodePolish(body("晚上读朋友推荐的新书，很喜欢勇敢面对困难的故事。"), p); err != nil {
		t.Fatal("stable topic outside the rewrite targets was mistaken for an omission", err)
	}
	if _, err := decodePolish(body("今天商量周末去公园散步。"), p); !errors.Is(err, ErrInvalid) {
		t.Fatal("new topic missing from both output and context accepted", err)
	}
}

func TestPolishRejectsRepeatedReadOnlyContextEvenIfNewTopicIsPresent(t *testing.T) {
	p := *polishFixture().Polish
	p.Context = []Block{{ID: uuid.NewString(), Style: "body", Text: "今天商量周末去公园散步。"}}
	raw, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: "今天商量周末去公园散步。", Style: "body", TargetIDs: []string{p.Targets[0].ID}}, {Text: "晚上读朋友推荐的新书。", Style: "body", TargetIDs: []string{p.Targets[0].ID}}}, "questions": []string{}})
	if _, err := decodePolish(string(raw), p); !errors.Is(err, errPolishRepeatsContext) {
		t.Fatal("model repeated stable context", err)
	}
}
