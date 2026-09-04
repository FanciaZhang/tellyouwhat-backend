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
func TestSocketReceiptsResumeAndFinalRevisionAcknowledgement(t *testing.T) {
	speech := &scriptedSpeech{}
	model := &scriptedRewriter{}
	s := &Service{Store: NewMemoryStore(), Speech: speech, Model: model, Secret: make([]byte, 32)}
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
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, PCM: make([]byte, 6400), Final: true})
	var event Event
	if err := websocket.JSON.Receive(ws, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "receipt" || event.Receipt.SHA256 != receipt.SHA256 {
		t.Fatalf("%+v", event)
	}
	if speech.opens.Load() != 1 {
		t.Fatal("duplicate segment reached paid provider")
	}
	start, _ := Period(identity.Anchor, time.Now())
	remaining, _ := s.Store.Remaining(context.Background(), identity.Owner, start.Format(time.RFC3339), MonthlyMilliseconds)
	if remaining != MonthlyMilliseconds-200 {
		t.Fatal(remaining)
	}
	if model.calls.Load() != 1 {
		t.Fatal("unchanged snapshot triggered repeated rewrite", model.calls.Load())
	}
}
