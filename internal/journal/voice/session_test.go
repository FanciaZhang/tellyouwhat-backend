package voice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
	if err := receiveVoiceResult(ws, &event); err != nil {
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
	result    chan Transcript
	closed    chan struct{}
	autoFinal bool
}

func (s *scriptedSpeech) Open(context.Context, []string) (SpeechConnection, error) {
	s.opens.Add(1)
	return &scriptedConnection{result: make(chan Transcript, 1), closed: make(chan struct{}), autoFinal: true}, nil
}
func (c *scriptedConnection) Send(_ []byte, final bool) error {
	if final && c.autoFinal {
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
	return RewriteResult{Revision: scriptedRevision(s, tr)}, nil
}

func acknowledgeTestRevision(s *Snapshot, r *Revision) {
	consumed := map[string]bool{}
	for _, id := range r.ConsumedSourceIDs {
		consumed[id] = true
		s.KnownSourceIDs = append(s.KnownSourceIDs, id)
	}
	pending := make([]SourceUtterance, 0, len(s.PendingUtterances))
	for _, u := range s.PendingUtterances {
		if !consumed[u.ID] {
			pending = append(pending, u)
		}
	}
	s.PendingUtterances = pending
	s.SemanticState = r.SemanticState
}

func scriptedRevision(s Snapshot, tr int) Revision {
	ids := make([]string, 0, len(s.PendingUtterances))
	text := ""
	for _, utterance := range s.PendingUtterances {
		ids = append(ids, utterance.ID)
		text += utterance.Text
	}
	passages := []Passage{}
	if text != "" {
		passages = append(passages, Passage{BlockID: s.Blocks[0].ID, SourceIDs: ids})
	}
	return Revision{BaseRevision: s.Revision, TranscriptRevision: tr,
		BlockEdits: []BlockEdit{{Kind: "replace", ID: s.Blocks[0].ID, Text: text, Style: "body"}}, Passages: passages,
		ConsumedSourceIDs: ids, SemanticState: s.SemanticState, Questions: []string{}}
}

type delayedRewriter struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

type queuedSpeechRewriter struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (r *queuedSpeechRewriter) Rewrite(ctx context.Context, s Snapshot, tr int) (RewriteResult, error) {
	if r.calls.Add(1) == 1 && r.started != nil {
		close(r.started)
		select {
		case <-r.release:
		case <-ctx.Done():
			return RewriteResult{}, ctx.Err()
		}
	}
	revision := scriptedRevision(s, tr)
	revision.BlockEdits[0].Text = s.Blocks[0].Text + revision.BlockEdits[0].Text
	return RewriteResult{Revision: revision}, nil
}

func TestNewSpeechDoesNotDiscardInFlightRewrite(t *testing.T) {
	model := &queuedSpeechRewriter{started: make(chan struct{}), release: make(chan struct{})}
	s := &Service{Store: NewMemoryStore(), Model: model, Secret: make([]byte, 32)}
	session := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Serve(w, r, session) }))
	defer server.Close()
	ticket, err := s.Issue(context.Background(), Identity{Owner: "continuous-speech", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, session)
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
		if err := receiveVoiceResult(ws, &event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	send := func(frame Frame) {
		t.Helper()
		if err := websocket.JSON.Send(ws, frame); err != nil {
			t.Fatal(err)
		}
	}
	read()
	first := SourceUtterance{ID: uuid.NewString(), Text: "第一段口述。"}
	second := SourceUtterance{ID: uuid.NewString(), Text: "第二段口述。"}
	snapshot := Snapshot{Blocks: []Block{{ID: uuid.NewString(), Text: "原来已有的文字。", Style: "body"}},
		Transcript: first.Text, PendingUtterances: []SourceUtterance{first}}
	send(Frame{Type: "snapshot", Snapshot: &snapshot})
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("first batch did not start")
	}
	// Another receipt's client snapshot arrives while the first model call runs.
	snapshot.Transcript += second.Text
	snapshot.PendingUtterances = append(snapshot.PendingUtterances, second)
	send(Frame{Type: "snapshot", Snapshot: &snapshot})
	send(Frame{Type: "finish"})
	send(Frame{Type: "ping"})
	if event := read(); event.Type != "pong" {
		t.Fatal(event)
	}
	close(model.release)
	for _, source := range []SourceUtterance{first, second} {
		event := read()
		if event.Type != "revision" || !slices.Equal(event.Revision.ConsumedSourceIDs, []string{source.ID}) {
			t.Fatalf("speech append discarded or duplicated the frozen batch: %+v", event)
		}
		snapshot.Revision++
		snapshot.Blocks[0].Text = event.Revision.BlockEdits[0].Text
		acknowledgeTestRevision(&snapshot, event.Revision)
		send(Frame{Type: "snapshot", Snapshot: &snapshot})
	}
	if event := read(); event.Type != "finished" {
		t.Fatal(event)
	}
	if snapshot.Blocks[0].Text != "原来已有的文字。第一段口述。第二段口述。" || model.calls.Load() != 2 {
		t.Fatalf("body=%q calls=%d", snapshot.Blocks[0].Text, model.calls.Load())
	}
}

func TestLongPendingSpeechDrainsInBoundedBatchesWithoutLosingExistingBody(t *testing.T) {
	model := &queuedSpeechRewriter{}
	s := &Service{Store: NewMemoryStore(), Model: model, Secret: make([]byte, 32)}
	session := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Serve(w, r, session) }))
	defer server.Close()
	ticket, err := s.Issue(context.Background(), Identity{Owner: "catch-up", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, session)
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
	if err := receiveVoiceResult(ws, &event); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Blocks: []Block{{ID: uuid.NewString(), Text: "已有正文。", Style: "body"}}}
	wanted := snapshot.Blocks[0].Text
	for i := 0; i < 25; i++ {
		text := strings.Repeat("完整保留口述细节。", 10)
		snapshot.PendingUtterances = append(snapshot.PendingUtterances, SourceUtterance{ID: uuid.NewString(), Text: text})
		snapshot.Transcript += text
		wanted += text
	}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	consumed := map[string]bool{}
	for {
		if err := receiveVoiceResult(ws, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "finished" {
			break
		}
		if event.Type != "revision" {
			t.Fatal(event)
		}
		revision := event.Revision
		if len(revision.ConsumedSourceIDs) > rewriteBatchSources {
			t.Fatal("unbounded sources")
		}
		for _, id := range revision.ConsumedSourceIDs {
			if consumed[id] {
				t.Fatal("source consumed twice")
			}
			consumed[id] = true
		}
		snapshot.Revision++
		snapshot.Blocks[0].Text = revision.BlockEdits[0].Text
		acknowledgeTestRevision(&snapshot, revision)
		if err := websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot}); err != nil {
			t.Fatal(err)
		}
	}
	if len(consumed) != 25 || snapshot.Blocks[0].Text != wanted || model.calls.Load() < 4 {
		t.Fatalf("consumed=%d calls=%d body_matches=%v", len(consumed), model.calls.Load(), snapshot.Blocks[0].Text == wanted)
	}
}

func TestPendingSourceAppendKeepsOldEvidenceButEditsInvalidateIt(t *testing.T) {
	first := SourceUtterance{ID: uuid.NewString(), Text: "已确认口述。"}
	second := SourceUtterance{ID: uuid.NewString(), Text: "新增口述。"}
	if !preservesPendingSources([]SourceUtterance{first}, []SourceUtterance{first, second}) {
		t.Fatal("append invalidated committed source")
	}
	changed := first
	changed.Person = "用户刚指定的人物"
	if preservesPendingSources([]SourceUtterance{first}, []SourceUtterance{changed, second}) {
		t.Fatal("identity edit reused stale source")
	}
	if preservesPendingSources([]SourceUtterance{first}, []SourceUtterance{second}) {
		t.Fatal("removed source reused stale result")
	}
	if preservesPendingSources([]SourceUtterance{first, second}, []SourceUtterance{second, first}) {
		t.Fatal("reordered source reused stale result")
	}
	long := first
	long.Text = strings.Repeat("长", rewriteBatchCharacters+1)
	if batch := pendingRewriteBatch([]SourceUtterance{long, second}); len(batch) != 1 || batch[0] != long {
		t.Fatal("indivisible source was truncated")
	}
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
	return RewriteResult{Revision: scriptedRevision(s, tr)}, nil
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
	if err = receiveVoiceResult(ws, &event); err != nil {
		t.Fatal(err)
	}
	sourceID := uuid.NewString()
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), "", ""}}, Transcript: "完整口述",
		PendingUtterances: []SourceUtterance{{ID: sourceID, Text: "完整口述"}}}
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
	if err = receiveVoiceResult(ws, &event); err != nil || event.Type != "pong" {
		t.Fatalf("%+v %v", event, err)
	}
	close(model.release)
	if err = receiveVoiceResult(ws, &event); err != nil || event.Type != "revision" {
		t.Fatalf("finished before applying: %+v %v", event, err)
	}
	snapshot.Revision++
	snapshot.Blocks[0].Text = event.Revision.BlockEdits[0].Text
	acknowledgeTestRevision(&snapshot, event.Revision)
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	if err = receiveVoiceResult(ws, &event); err != nil || event.Type != "finished" {
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
		if err = receiveVoiceResult(ws, &ready); err != nil || ready.Type != "ready" {
			t.Fatalf("%+v %v", ready, err)
		}
		return ws
	}
	ws := dial()
	snapshot := Snapshot{Blocks: []Block{{block, "", ""}}, Words: []string{}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, PCM: make([]byte, 6400), Final: true})
	var receipt *Receipt
	for receipt == nil {
		var event Event
		if err := receiveVoiceResult(ws, &event); err != nil {
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
		if err := receiveVoiceResult(ws, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "revision" {
			snapshot.Blocks[0].Text = event.Revision.BlockEdits[0].Text
			acknowledgeTestRevision(&snapshot, event.Revision)
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
		if err := receiveVoiceResult(ws, &event); err != nil {
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

// Interim recognition is UI-only. A completed source segment triggers exactly
// one bounded rewrite without waiting for the lease heartbeat.
type streamingSpeech struct{ connection *scriptedConnection }

func (s streamingSpeech) Open(context.Context, []string) (SpeechConnection, error) {
	return s.connection, nil
}
func TestOnlyFinalSpeechTriggersOneIncrementalRewrite(t *testing.T) {
	conn := &scriptedConnection{result: make(chan Transcript, 4), closed: make(chan struct{})}
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
	if err := receiveVoiceResult(ws, &event); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), "", ""}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	segment := uuid.NewString()
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, PCM: make([]byte, 6400)})
	conn.result <- Transcript{Text: "今天去了公园。", Stable: "今天去了公园。"}
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "transcript" {
		t.Fatalf("%+v %v", event, err)
	}
	select {
	case <-model.started:
		t.Fatal("interim speech spent rewrite tokens")
	case <-time.After(400 * time.Millisecond):
	}
	conn.result <- Transcript{Text: "今天去了公园。后来去了湖边。", Stable: "今天去了公园。"}
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "transcript" {
		t.Fatalf("interim transcript missing: %+v %v", event, err)
	}
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, Final: true})
	time.Sleep(20 * time.Millisecond)
	conn.result <- Transcript{Text: "今天去了公园。后来去了湖边。", Stable: "今天去了公园。后来去了湖边。", Final: true}
	if event = func() Event { var value Event; _ = receiveVoiceResult(ws, &value); return value }(); event.Type != "transcript" {
		t.Fatalf("final transcript missing: %+v", event)
	}
	if event = func() Event { var value Event; _ = receiveVoiceResult(ws, &value); return value }(); event.Type != "receipt" {
		t.Fatalf("receipt missing: %+v", event)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("final source did not schedule rewrite")
	}
	websocket.JSON.Send(ws, Frame{Type: "ping"})
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "pong" {
		t.Fatalf("slow rewrite blocked transport: %+v %v", event, err)
	}
	close(model.release)
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "revision" {
		t.Fatalf("%+v %v", event, err)
	}
	if got := event.Revision.BlockEdits[0].Text; got != "今天去了公园。后来去了湖边。" {
		t.Fatal("final source was not organized", got)
	}
	if model.calls.Load() != 1 {
		t.Fatal(model.calls.Load())
	}
}

func TestLateRevisionAcknowledgementCannotEraseCommittedSpeech(t *testing.T) {
	conn := &scriptedConnection{result: make(chan Transcript, 4), closed: make(chan struct{})}
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
		if err := receiveVoiceResult(ws, &event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	read()
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), "", ""}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, PCM: make([]byte, 6400)})
	conn.result <- Transcript{Text: "今天见到一个朋友。", Stable: "今天见到一个朋友。"}
	if e := read(); e.Type != "transcript" {
		t.Fatal(e)
	}
	// A snapshot already queued by the client legitimately has no knowledge of
	// the receipt that is about to be committed.
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, Final: true})
	time.Sleep(20 * time.Millisecond)
	conn.result <- Transcript{Text: "今天见到一个朋友。", Stable: "今天见到一个朋友。", Final: true}
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
	if got := final.Revision.BlockEdits[0].Text; got != receipt.Text {
		t.Fatalf("old ACK erased final ASR: got %q want %q", got, receipt.Text)
	}
	snapshot.Revision++
	snapshot.Transcript = receipt.Text
	snapshot.Blocks[0].Text = final.Revision.BlockEdits[0].Text
	acknowledgeTestRevision(&snapshot, final.Revision)
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
		if err := receiveVoiceResult(ws, &event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	read()
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), "", ""}}}
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
	if event.Type != "revision" || event.Revision.BlockEdits[0].Text != receipt.Text {
		t.Fatalf("replayed speech lost or duplicated: %+v", event)
	}
	if speech.opens.Load() != 0 {
		t.Fatal("duplicate receipt reopened speech provider")
	}
	snapshot.Revision++
	snapshot.Transcript = receipt.Text
	snapshot.Blocks[0].Text = receipt.Text
	acknowledgeTestRevision(&snapshot, event.Revision)
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	if event := read(); event.Type != "finished" {
		t.Fatal(event)
	}
}

func TestManualEditAtExpectedAcknowledgementRevisionRewritesLatestBody(t *testing.T) {
	model := &scriptedRewriter{}
	s := &Service{Store: NewMemoryStore(), Speech: &scriptedSpeech{}, Model: model, Secret: make([]byte, 32)}
	session := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Serve(w, r, session) }))
	defer server.Close()
	ticket, err := s.Issue(context.Background(), Identity{Owner: "typing-race", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, session)
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
		var e Event
		if err := receiveVoiceResult(ws, &e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	read()
	snapshot := Snapshot{Blocks: []Block{{ID: uuid.NewString(), Text: "已有正文"}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: uuid.NewString(), PCM: make([]byte, 6400), Final: true})
	for {
		if e := read(); e.Type == "receipt" {
			snapshot.Transcript = e.Receipt.Text
			break
		}
	}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	first := read()
	if first.Type != "revision" {
		t.Fatal(first)
	}
	// The user typed while this result was in flight; the client rejects it.
	snapshot.Revision = first.Revision.BaseRevision + 1
	snapshot.Blocks[0].Text = "刚刚手动补充的内容"
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	next := read()
	if next.Type != "revision" || next.Revision.BaseRevision != snapshot.Revision {
		t.Fatalf("typing was mistaken for an ACK: %+v", next)
	}
	snapshot.Revision++
	snapshot.Blocks[0].Text = next.Revision.BlockEdits[0].Text
	acknowledgeTestRevision(&snapshot, next.Revision)
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	if e := read(); e.Type != "finished" {
		t.Fatal(e)
	}
	if model.calls.Load() != 2 {
		t.Fatal(model.calls.Load())
	}
}

// Receive the next result while allowing independent processing notifications.
func receiveVoiceResult(ws *websocket.Conn, destination any) error {
	for {
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err != nil {
			return err
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return err
		}
		if envelope.Type == "processing" {
			continue
		}
		return json.Unmarshal(data, destination)
	}
}
