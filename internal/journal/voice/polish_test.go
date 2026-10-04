package voice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

func polishFixture() Snapshot {
	id := uuid.NewString()
	return Snapshot{DictationMode: true, WritingStyle: StyleDocumentary, Blocks: []Block{{ID: id, Text: "今天，嗯，去了河边。", Style: "body"}}, Polish: &PolishRequest{Targets: []PolishTarget{{ID: id, Text: "今天，嗯，去了河边。", SourceText: "今天，嗯，去了河边。", SourceIDs: []string{uuid.NewString()}}}, Context: []Block{}}}
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
	if strings.Contains(string(prepared.Body), "diagramCreations") || strings.Contains(string(prepared.Body), "semanticState") || len(prepared.Body) > 8000 {
		t.Fatal("ordinary task still carries structural context")
	}
	if body["max_output_tokens"].(float64) > 2048 || prepared.TimeoutSeconds > 25 || prepared.Parameters.ReasoningEffort != "disabled" {
		t.Fatal("unbounded polish parameters")
	}
}
func TestPolishBaselineIsServerOwnedAndUnknownParagraphsAreRejected(t *testing.T) {
	s := polishFixture()
	target := s.Polish.Targets[0]
	output := `{"paragraphs":[{"targetIDs":["` + target.ID + `"],"text":"今天去了河边。"}],"questions":[]}`
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
	second := PolishTarget{ID: uuid.NewString(), Text: "啊，我更想先去图书馆还书。", SourceText: "啊，我更想先去图书馆还书。", SourceIDs: []string{uuid.NewString()}}
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
	output := `{"paragraphs":[{"targetIDs":["` + s.Polish.Targets[0].ID + `","` + second.ID + `"],"text":"我想去河边，妻子更想先去图书馆还书。"}],"questions":[]}`
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
	if _, err := decodePolish(`{"paragraphs":[{"targetIDs":["`+p.Targets[0].ID+`"],"text":"去了河边。"}],"questions":[]}`, p); err == nil {
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
	result, err := decodePolish(`{"paragraphs":[{"targetIDs":["`+p.Targets[0].ID+`"],"text":"今天去了河边。\n\n在那里散步。"}],"questions":[]}`, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Paragraphs) != 2 || result.Paragraphs[1].Text != "在那里散步。" || result.Paragraphs[1].TargetIDs[0] != p.Targets[0].ID {
		t.Fatal("normalized paragraphs lost source coverage")
	}
}

func TestNarrativeActorCorrectionAcknowledgesPriorInFlightResult(t *testing.T) {
	p := *polishFixture().Polish
	p.Targets[0].Turns = []SourceUtterance{{ID: p.Targets[0].SourceIDs[0], Text: p.Targets[0].SourceText, Speaker: "segment:1"}}
	prior := append([]PolishTarget(nil), p.Targets...)
	p.Targets[0].Turns = append([]SourceUtterance(nil), p.Targets[0].Turns...)
	p.Targets[0].Turns[0].Person = "妻子"
	if !polishAcknowledged(&p, prior) {
		t.Fatal("a corrected actor must allow a fresh narrative request")
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
	output := `{"paragraphs":[{"targetIDs":["` + p.Targets[0].ID + `"],"text":"今天去了河边散步。"}],"questions":[]}`
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
	if input.Targets[0].RetainedText != target.RetainedText || input.Targets[0].SourceKeys[0] != target.SourceKeys[0] {
		t.Fatal("lost continuous paragraph prefix or client source identity")
	}
	result, err := decodePolish(`{"paragraphs":[{"targetIDs":["`+target.ID+`"],"text":"上午和妻子去了公园，随后去河边。"}],"questions":[]}`, *s.Polish)
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
	return RewriteResult{Polish: &PolishRevision{Targets: s.Polish.Targets, Paragraphs: []PolishParagraph{{TargetIDs: []string{target.ID}, Text: "今天去了河边。"}}, Questions: []string{}}}, nil
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
	if err := websocket.JSON.Receive(ws, &ready); err != nil || ready.Type != "ready" {
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
	if err := websocket.JSON.Receive(ws, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "polish" || len(event.Polish.Targets) != 1 {
		t.Fatalf("lost completed prose: %+v", event)
	}
	// Remove just the processed baseline. The next sentence becomes its own job.
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
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Type != "rewrite_error" {
		t.Fatalf("polish failure: %+v %v", event, err)
	}
	snapshot.Polish.Paused = true
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: uuid.NewString(), PCM: make([]byte, 6400), Final: true})
	for event.Type != "receipt" {
		if err := websocket.JSON.Receive(ws, &event); err != nil {
			t.Fatal("polish failure killed ASR", err)
		}
	}
	if event.Receipt.Text == "" || speech.opens.Load() != 1 {
		t.Fatal("missing durable speech receipt")
	}
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Type != "finished" {
		t.Fatalf("paused finalization: %+v %v", event, err)
	}
}
