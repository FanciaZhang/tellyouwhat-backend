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
	output := `{"edits":[{"id":"` + target.ID + `","text":"今天去了河边。"}],"questions":[]}`
	result, err := decodePolish(output, *s.Polish)
	if err != nil {
		t.Fatal(err)
	}
	if result.Edits[0].ExpectedText != target.Text || result.Targets[0].SourceText != target.SourceText {
		t.Fatal("lost immutable baseline")
	}
	if _, err = decodePolish(strings.Replace(output, target.ID, uuid.NewString(), 1), *s.Polish); err == nil {
		t.Fatal("accepted unrelated paragraph")
	}
	if _, err = decodePolish(output+" {}", *s.Polish); err == nil {
		t.Fatal("accepted multiple outputs")
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
	return RewriteResult{Polish: &PolishRevision{Targets: s.Polish.Targets, Edits: []PolishEdit{{ID: target.ID, ExpectedText: target.Text, Text: "今天去了河边。"}}, Questions: []string{}}}, nil
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
