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
	// An actual document edit invalidates the old model result.
	snapshot.Revision++
	snapshot.Blocks[0].Text = "用户刚刚修改了正文"
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
	if model.calls.Load() != 1 {
		t.Fatal("unchanged acknowledgement restarted rewriting", model.calls.Load())
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
	// The new recognition may already start its immediate rewrite. It must
	// not create more than one call for that newly recognized source.
	if model.calls.Load() > 2 {
		t.Fatal("duplicate rewrite", model.calls.Load())
	}
}

// Streaming results must schedule work without waiting for the lease heartbeat.
// New speech received during a slow model call is coalesced until the client ACK.
type streamingSpeech struct{ connection *scriptedConnection }

func (s streamingSpeech) Open(context.Context, []string) (SpeechConnection, error) {
	return s.connection, nil
}
func TestStableSpeechRewritesImmediatelyAndCatchesUpAfterAcknowledgement(t *testing.T) {
	conn := &scriptedConnection{make(chan Transcript, 4), make(chan struct{})}
	model := &delayedRewriter{started: make(chan struct{}), release: make(chan struct{})}
	s := &Service{Store: NewMemoryStore(), Speech: streamingSpeech{conn}, Model: model, Secret: make([]byte, 32)}
	session := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Serve(w, r, session) }))
	defer server.Close()
	ticket, err := s.Issue(context.Background(), Identity{Owner: "streaming", Anchor: time.Now().AddDate(0, -1, 0), ExpiresAt: time.Now().Add(time.Hour)}, session)
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
	ws.SetDeadline(time.Now().Add(4 * time.Second))
	var event Event
	if err := websocket.JSON.Receive(ws, &event); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), ""}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: uuid.NewString(), PCM: make([]byte, 6400)})
	conn.result <- Transcript{Text: "今天去了公园。", Stable: "今天去了公园。"}
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Type != "transcript" {
		t.Fatalf("%+v %v", event, err)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("stable speech waited for five-second heartbeat")
	}
	conn.result <- Transcript{Text: "今天去了公园。后来去了湖边。", Stable: "今天去了公园。"}
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Type != "transcript" {
		t.Fatalf("slow rewrite blocked ASR: %+v %v", event, err)
	}
	websocket.JSON.Send(ws, Frame{Type: "ping"})
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Type != "pong" {
		t.Fatalf("slow rewrite blocked transport: %+v %v", event, err)
	}
	close(model.release)
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Type != "revision" {
		t.Fatalf("%+v %v", event, err)
	}
	snapshot.Revision++
	snapshot.Blocks[0].Text = event.Revision.Patches[0].Text
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	if err := websocket.JSON.Receive(ws, &event); err != nil || event.Type != "revision" {
		t.Fatalf("latest speech waited for heartbeat: %+v %v", event, err)
	}
	if got := event.Revision.Patches[0].Text; got != "今天去了公园。后来去了湖边。" {
		t.Fatal("interim words must reach the rewrite before final ASR confirmation", got)
	}
	if model.calls.Load() != 2 {
		t.Fatal(model.calls.Load())
	}
}

func TestLateRevisionAcknowledgementCannotEraseCommittedSpeech(t *testing.T) {
	conn := &scriptedConnection{make(chan Transcript, 4), make(chan struct{})}
	s := &Service{Store: NewMemoryStore(), Speech: streamingSpeech{conn}, Model: &scriptedRewriter{}, Secret: make([]byte, 32)}
	session, segment := uuid.NewString(), uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Serve(w, r, session) }))
	defer server.Close()
	ticket, err := s.Issue(context.Background(), Identity{Owner: "receipt-race", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, session)
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
	read := func() Event {
		t.Helper()
		var event Event
		if err := websocket.JSON.Receive(ws, &event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	read()
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), ""}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, PCM: make([]byte, 6400)})
	conn.result <- Transcript{Text: "今天见到一个朋友。", Stable: "今天见到一个朋友。"}
	if e := read(); e.Type != "transcript" {
		t.Fatal(e)
	}
	provisional := read()
	if provisional.Type != "revision" {
		t.Fatal(provisional)
	}
	// The client starts an ACK before receiving the final receipt. Its network
	// write arrives later, so this snapshot legitimately still has no transcript.
	snapshot.Revision++
	snapshot.Blocks[0].Text = provisional.Revision.Patches[0].Text
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, Final: true})
	var receipt *Receipt
	for receipt == nil {
		e := read()
		if e.Type == "error" {
			t.Fatal(e)
		}
		receipt = e.Receipt
	}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	final := read()
	if final.Type != "revision" {
		t.Fatal(final)
	}
	if got := final.Revision.Patches[0].Text; got != receipt.Text {
		t.Fatalf("old ACK erased final ASR: got %q want %q", got, receipt.Text)
	}
	snapshot.Revision++
	snapshot.Transcript = receipt.Text
	snapshot.Blocks[0].Text = final.Revision.Patches[0].Text
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	if e := read(); e.Type != "finished" {
		t.Fatal(e)
	}
}

func TestReplayedReceiptSeedsCanonicalTranscriptOnce(t *testing.T) {
	store := NewMemoryStore()
	owner, session, segment := "receipt-replay", uuid.NewString(), uuid.NewString()
	now := time.Now()
	period, _ := Period(now, now)
	pcm := make([]byte, 6400)
	receipt := Receipt{SegmentID: segment, SHA256: hash(string(pcm)), Text: "已经确认的原始转写。", Milliseconds: 200}
	if err := store.Lock(context.Background(), owner, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(context.Background(), owner, session, period.Format(time.RFC3339), "seed", receipt, MonthlyMilliseconds); err != nil {
		t.Fatal(err)
	}
	store.Unlock(context.Background(), owner, "seed")
	speech := &scriptedSpeech{}
	service := &Service{Store: store, Speech: speech, Model: &scriptedRewriter{}, Secret: make([]byte, 32)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { service.Serve(w, r, session) }))
	defer server.Close()
	ticket, err := service.Issue(context.Background(), Identity{Owner: owner, Anchor: now, ExpiresAt: now.Add(time.Hour)}, session)
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
	ws.SetDeadline(now.Add(3 * time.Second))
	read := func() Event {
		t.Helper()
		var event Event
		if err := websocket.JSON.Receive(ws, &event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	read()
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), ""}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	// Repeat a lost receipt twice; neither a second provider call nor duplicated
	// source text may result, even before a receipt snapshot gets back to the server.
	for i := 0; i < 2; i++ {
		websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, PCM: pcm, Final: true})
		if event := read(); event.Type != "receipt" {
			t.Fatal(event)
		}
	}
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	event := read()
	if event.Type != "revision" || event.Revision.Patches[0].Text != receipt.Text {
		t.Fatalf("replayed speech lost or duplicated: %+v", event)
	}
	if speech.opens.Load() != 0 {
		t.Fatal("duplicate receipt reopened speech provider")
	}
	snapshot.Revision++
	snapshot.Transcript = receipt.Text
	snapshot.Blocks[0].Text = receipt.Text
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	if event := read(); event.Type != "finished" {
		t.Fatal(event)
	}
}
