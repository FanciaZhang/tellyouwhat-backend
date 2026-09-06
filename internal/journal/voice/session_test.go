package voice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

type scriptedSpeech struct{ opens atomic.Int32 }

func TestBusySubscriptionDeliversAnActionableSocketError(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	identity := Identity{Owner: "other-device", Anchor: time.Now().AddDate(0, -1, 0), ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.Lock(ctx, identity.Owner, "existing-connection"); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, Secret: make([]byte, 32)}
	session := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Serve(w, r, session) }))
	defer server.Close()
	ticket, err := s.Issue(ctx, identity, session)
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
	ws.SetDeadline(time.Now().Add(3 * time.Second))
	var event Event
	if err := websocket.JSON.Receive(ws, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "error" || event.Code != "voice_session_busy" {
		t.Fatalf("%+v", event)
	}
	if err := store.Renew(ctx, identity.Owner, "existing-connection"); err != nil {
		t.Fatal("refusal removed another device's lease", err)
	}
}

type scriptedConnection struct {
	result chan Transcript
	closed chan struct{}
}

func (s *scriptedSpeech) Open(context.Context, []string) (SpeechConnection, error) {
	s.opens.Add(1)
	return &scriptedConnection{make(chan Transcript, 1), make(chan struct{})}, nil
}
func (c *scriptedConnection) Send(_ []byte, final bool) error {
	if final {
		c.result <- Transcript{Text: "今天见到许知远。", Stable: "今天见到许知远。", Final: true}
	}
	return nil
}
func (c *scriptedConnection) Receive() (Transcript, error) {
	select {
	case result := <-c.result:
		return result, nil
	case <-c.closed:
		return Transcript{}, io.EOF
	}
}
func (c *scriptedConnection) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

type scriptedRewriter struct{ calls atomic.Int32 }

func (r *scriptedRewriter) Rewrite(_ context.Context, s Snapshot, tr int) (RewriteResult, error) {
	r.calls.Add(1)
	return RewriteResult{Revision: Revision{BaseRevision: s.Revision, TranscriptRevision: tr, Patches: []Patch{{ID: s.Blocks[0].ID, Text: s.Transcript}}, Questions: []string{}}}, nil
}

type delayedRewriter struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (r *delayedRewriter) Rewrite(ctx context.Context, s Snapshot, tr int) (RewriteResult, error) {
	if r.calls.Add(1) == 1 {
		close(r.started)
		select {
		case <-r.release:
		case <-ctx.Done():
			return RewriteResult{}, ctx.Err()
		}
	}
	return RewriteResult{Revision: Revision{BaseRevision: s.Revision, TranscriptRevision: tr, Patches: []Patch{{ID: s.Blocks[0].ID, Text: s.Transcript}}}}, nil
}
func TestInterveningSnapshotCannotFinishWithoutAnAppliedRevision(t *testing.T) {
	model := &delayedRewriter{started: make(chan struct{}), release: make(chan struct{})}
	s := &Service{Store: NewMemoryStore(), Speech: &scriptedSpeech{}, Model: model, Secret: make([]byte, 32)}
	session := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Serve(w, r, session) }))
	defer server.Close()
	ctx := context.Background()
	ticket, err := s.Issue(ctx, Identity{Owner: "paid", Anchor: time.Now().AddDate(0, -1, 0), ExpiresAt: time.Now().Add(time.Hour)}, session)
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
	ws.SetDeadline(time.Now().Add(10 * time.Second))
	var event Event
	if err = websocket.JSON.Receive(ws, &event); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), ""}}, Transcript: "完整口述"}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	// A receipt acknowledgement can resend the same source while rewriting.
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "ping"})
	if err = websocket.JSON.Receive(ws, &event); err != nil || event.Type != "pong" {
		t.Fatalf("%+v %v", event, err)
	}
	close(model.release)
	if err = websocket.JSON.Receive(ws, &event); err != nil || event.Type != "revision" {
		t.Fatalf("finished before applying: %+v %v", event, err)
	}
	snapshot.Revision++
	snapshot.Blocks[0].Text = event.Revision.Patches[0].Text
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	if err = websocket.JSON.Receive(ws, &event); err != nil || event.Type != "finished" {
		t.Fatalf("%+v %v", event, err)
	}
	if model.calls.Load() != 2 {
		t.Fatal(model.calls.Load())
	}
}
func TestSocketReceiptsResumeAndFinalRevisionAcknowledgement(t *testing.T) {
	speech := &scriptedSpeech{}
	model := &scriptedRewriter{}
	s := &Service{Store: NewMemoryStore(), Speech: speech, Model: model, Secret: make([]byte, 32), Limit: 200}
	session := uuid.NewString()
	segment := uuid.NewString()
	block := uuid.NewString()
	identity := Identity{Owner: "subscription", Anchor: time.Now().AddDate(0, -1, 0), ExpiresAt: time.Now().Add(time.Hour)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Serve(w, r, session) }))
	defer server.Close()
	dial := func() *websocket.Conn {
		t.Helper()
		ticket, err := s.Issue(context.Background(), identity, session)
		if err != nil {
			t.Fatal(err)
		}
		config, _ := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http"), "http://localhost")
		config.Header.Set("Authorization", "Bearer "+ticket.Token)
		ws, err := websocket.DialConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		ws.SetDeadline(time.Now().Add(10 * time.Second))
		var ready Event
		if err = websocket.JSON.Receive(ws, &ready); err != nil || ready.Type != "ready" {
			t.Fatalf("%+v %v", ready, err)
		}
		return ws
	}
	ws := dial()
	snapshot := Snapshot{Blocks: []Block{{block, ""}}, Words: []string{}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, PCM: make([]byte, 6400), Final: true})
	var receipt *Receipt
	for receipt == nil {
		var event Event
		if err := websocket.JSON.Receive(ws, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "receipt" {
			receipt = event.Receipt
		}
	}
	if receipt.Milliseconds != 200 {
		t.Fatal(receipt)
	}
	snapshot.Transcript = receipt.Text
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	for {
		var event Event
		if err := websocket.JSON.Receive(ws, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "revision" {
			snapshot.Blocks[0].Text = event.Revision.Patches[0].Text
			snapshot.Revision++
			websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
		}
		if event.Type == "finished" {
			break
		}
	}
	ws.Close()
	// The preceding handler has released its fenced lease when it closes.
	deadline := time.Now().Add(time.Second)
	for {
		err := s.Store.Lock(context.Background(), identity.Owner, "probe")
		if err == nil {
			s.Store.Unlock(context.Background(), identity.Owner, "probe")
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	ws = dial()
	defer ws.Close()
	// A second device can have the audio but not yet the final transcript.
	// Cleared receipts must regenerate real text even with no allowance left.
	snapshot.Transcript = ""
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, PCM: make([]byte, 6400), Final: true})
	var event Event
	for event.Type != "receipt" {
		if err := websocket.JSON.Receive(ws, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "error" {
			t.Fatalf("retry failed: %+v", event)
		}
	}
	if event.Receipt.SHA256 != receipt.SHA256 || event.Receipt.Text != receipt.Text {
		t.Fatalf("%+v", event)
	}
	if speech.opens.Load() != 2 {
		t.Fatal("forgotten transcript must be recognized again")
	}
	start, _ := Period(identity.Anchor, time.Now())
	remaining, _ := s.Store.Remaining(context.Background(), identity.Owner, start.Format(time.RFC3339), s.limit())
	if remaining != 0 {
		t.Fatal(remaining)
	}
	if model.calls.Load() != 1 {
		t.Fatal("unchanged snapshot triggered repeated rewrite", model.calls.Load())
	}
}
