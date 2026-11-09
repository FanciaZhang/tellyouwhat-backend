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
	if batch := pendingRewriteBatch([]SourceUtterance{long, second}); len(batch) != 1 || !batch[0].Equal(long) {
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
	speech, model := &scriptedSpeech{}, &scriptedRewriter{}
	service := &Service{Store: NewMemoryStore(), Speech: speech, Model: model, Secret: make([]byte, 32), Limit: 200}
	session, segment := uuid.NewString(), uuid.NewString()
	identity := Identity{Owner: "receipt-resume", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	ws := voiceLifecycleSocket(t, service, identity, session)
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), "", ""}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	receipt, completed := voiceLifecycleShort(t, ws, segment, uuid.NewString(), make([]byte, 6400))
	if receipt.Milliseconds != 200 || completed.Text == "" {
		t.Fatal(receipt, completed)
	}
	snapshot.Transcript = completed.Text
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	for {
		event := voiceLifecycleRead(t, ws)
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
		t.Fatal("unchanged ACK repeated rewrite", model.calls.Load())
	}
	ws.Close()
	deadline := time.Now().Add(time.Second)
	for {
		err := service.Store.Lock(context.Background(), identity.Owner, "probe")
		if err == nil {
			service.Store.Unlock(context.Background(), identity.Owner, "probe")
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	ws = voiceLifecycleSocket(t, service, identity, session)
	snapshot.Transcript = ""
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	retry, recognized := voiceLifecycleShort(t, ws, segment, uuid.NewString(), make([]byte, 6400))
	if retry.SHA256 != receipt.SHA256 || recognized.Text != completed.Text || speech.opens.Load() != 2 {
		t.Fatal("billed audio must regenerate transcript without double billing", retry, recognized, speech.opens.Load())
	}
	start, _ := Period(identity.Anchor, time.Now())
	remaining, _ := service.Store.Remaining(context.Background(), identity.Owner, start.Format(time.RFC3339), service.limit())
	if remaining != 0 {
		t.Fatal(remaining)
	}
}

func TestCanonicalTurnsKeepSpeakerAndTimingWhenFullASRTextAddsWhitespace(t *testing.T) {
	scope := uuid.NewString()
	provider := []Utterance{
		{Text: "我在湖边喝水。", StartMilliseconds: 0, EndMilliseconds: 1000, Definite: true, Speaker: "0"},
		{Text: "她去值班。", StartMilliseconds: 16000, EndMilliseconds: 19000, Definite: true, Speaker: "1"},
		{Text: "今天回家。", StartMilliseconds: 65000, EndMilliseconds: 68000, Definite: true, Speaker: "0"},
	}
	text := "我在湖边喝水。\u3000\n她去值班。\t今天回家。"
	got := identifiedUtterances(scope, text, provider, 70000)
	joined := ""
	for _, u := range got {
		joined += u.Text
	}
	if len(got) != 3 || joined != text || got[0].Speaker != "0" || got[1].Speaker != "1" || got[2].Speaker != "0" || got[2].StartMilliseconds != 65000 {
		t.Fatal("whitespace flattened actual ASR turns or lost authoritative text", got)
	}
	if provider[0].Text != "我在湖边喝水。" || provider[1].Text != "她去值班。" {
		t.Fatal("alignment mutated original provider evidence")
	}
	withEmpty := append([]Utterance{provider[0], {Text: "", StartMilliseconds: 2000, EndMilliseconds: 3000, Definite: true}}, provider[1:]...)
	if kept := identifiedUtterances(scope, text, withEmpty, 70000); len(kept) != 3 || kept[2].Speaker != "0" {
		t.Fatal("empty provider turn flattened all other speaker evidence", kept)
	}
	for i, u := range got {
		if _, err := uuid.Parse(u.ID); err != nil || (i > 0 && u.ID == got[i-1].ID) {
			t.Fatal("canonical source identity missing", err)
		}
	}
	changed := identifiedUtterances(scope, "我在湖边喝水，她去值班。今天回家。", provider, 70000)
	if len(changed) != 1 || changed[0].Speaker != "" {
		t.Fatal("substantive punctuation mismatch borrowed uncertain speaker evidence")
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
	service := &Service{Store: NewMemoryStore(), Speech: streamingSpeech{conn}, Model: model, Secret: make([]byte, 32)}
	ws := voiceLifecycleSocket(t, service, Identity{Owner: "streaming", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, uuid.NewString())
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), "", ""}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	segment, recognition := uuid.NewString(), uuid.NewString()
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, RecognitionID: recognition, PCM: make([]byte, 6400)})
	conn.result <- Transcript{Text: "今天去了公园。", Stable: "今天去了公园。"}
	if event := voiceLifecycleRead(t, ws); event.Type != "transcript" {
		t.Fatal(event)
	}
	select {
	case <-model.started:
		t.Fatal("interim source spent rewrite tokens")
	case <-time.After(400 * time.Millisecond):
	}
	conn.result <- Transcript{Text: "今天去了公园。后来去了湖边。", Stable: "今天去了公园。"}
	if event := voiceLifecycleRead(t, ws); event.Type != "transcript" {
		t.Fatal(event)
	}
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, RecognitionID: recognition, Final: true})
	if event := voiceLifecycleRead(t, ws); event.Type != "receipt" {
		t.Fatal("audio ACK should not await final ASR", event)
	}
	select {
	case <-model.started:
		t.Fatal("audio checkpoint closed semantic source")
	case <-time.After(30 * time.Millisecond):
	}
	websocket.JSON.Send(ws, Frame{Type: "recognition_finish", RecognitionID: recognition})
	time.Sleep(20 * time.Millisecond)
	conn.result <- Transcript{Text: "今天去了公园。后来去了湖边。", Stable: "今天去了公园。后来去了湖边。", Final: true}
	if event := voiceLifecycleRead(t, ws); event.Type != "recognition_completed" {
		t.Fatal(event)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("completed ASR did not schedule rewrite")
	}
	websocket.JSON.Send(ws, Frame{Type: "ping"})
	if event := voiceLifecycleRead(t, ws); event.Type != "pong" {
		t.Fatal("slow model blocked transport", event)
	}
	close(model.release)
	event := voiceLifecycleRead(t, ws)
	if event.Type != "revision" || event.Revision.BlockEdits[0].Text != "今天去了公园。后来去了湖边。" || model.calls.Load() != 1 {
		t.Fatal(event, model.calls.Load())
	}
}

func TestLateRevisionAcknowledgementCannotEraseCommittedSpeech(t *testing.T) {
	service := &Service{Store: NewMemoryStore(), Speech: &scriptedSpeech{}, Model: &scriptedRewriter{}, Secret: make([]byte, 32)}
	ws := voiceLifecycleSocket(t, service, Identity{Owner: "receipt-race", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, uuid.NewString())
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), "", ""}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	_, completed := voiceLifecycleShort(t, ws, uuid.NewString(), uuid.NewString(), make([]byte, 6400))
	// This pre-completion snapshot cannot replace server-confirmed raw text.
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	final := voiceLifecycleRead(t, ws)
	if final.Type != "revision" || final.Revision.BlockEdits[0].Text != completed.Text {
		t.Fatal("late ACK erased recognized speech", final)
	}
	snapshot.Revision++
	snapshot.Transcript = completed.Text
	snapshot.Blocks[0].Text = final.Revision.BlockEdits[0].Text
	acknowledgeTestRevision(&snapshot, final.Revision)
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	if event := voiceLifecycleRead(t, ws); event.Type != "finished" {
		t.Fatal(event)
	}
}

func TestReplayedAudioReceiptRecognizesOncePerConnectionWithoutDoubleBilling(t *testing.T) {
	store := NewMemoryStore()
	owner, session, segment := "receipt-replay", uuid.NewString(), uuid.NewString()
	now := time.Now()
	period, _ := Period(now, now)
	pcm := make([]byte, 6400)
	receipt := Receipt{SegmentID: segment, SHA256: hash(string(pcm)), Milliseconds: 200}
	store.Lock(context.Background(), owner, "seed")
	if _, err := store.Commit(context.Background(), owner, session, period.Format(time.RFC3339), "seed", receipt, MonthlyMilliseconds); err != nil {
		t.Fatal(err)
	}
	store.Unlock(context.Background(), owner, "seed")
	speech := &scriptedSpeech{}
	service := &Service{Store: store, Speech: speech, Model: &scriptedRewriter{}, Secret: make([]byte, 32)}
	ws := voiceLifecycleSocket(t, service, Identity{Owner: owner, Anchor: now, ExpiresAt: now.Add(time.Hour)}, session)
	snapshot := Snapshot{Blocks: []Block{{uuid.NewString(), "", ""}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	recognition := uuid.NewString()
	for range 2 {
		websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, RecognitionID: recognition, PCM: pcm, Final: true})
		if event := voiceLifecycleRead(t, ws); event.Type != "receipt" {
			t.Fatal(event)
		}
	}
	websocket.JSON.Send(ws, Frame{Type: "recognition_finish", RecognitionID: recognition})
	completed := voiceLifecycleUntil(t, ws, "recognition_completed")
	if speech.opens.Load() != 1 || completed.Milliseconds != 200 {
		t.Fatal("lost ACK duplicated provider input", speech.opens.Load(), completed)
	}
	remaining, _ := store.Remaining(context.Background(), owner, period.Format(time.RFC3339), MonthlyMilliseconds)
	if remaining != MonthlyMilliseconds-200 {
		t.Fatal("audio replay charged twice", remaining)
	}
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	event := voiceLifecycleRead(t, ws)
	if event.Type != "revision" || event.Revision.BlockEdits[0].Text != completed.Text {
		t.Fatal(event)
	}
	snapshot.Revision++
	snapshot.Transcript = completed.Text
	snapshot.Blocks[0].Text = completed.Text
	acknowledgeTestRevision(&snapshot, event.Revision)
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	if event := voiceLifecycleRead(t, ws); event.Type != "finished" {
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
	_, completed := voiceLifecycleShort(t, ws, uuid.NewString(), uuid.NewString(), make([]byte, 6400))
	snapshot.Transcript = completed.Text
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

func voiceLifecycleSocket(t *testing.T, service *Service, identity Identity, session string) *websocket.Conn {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { service.Serve(w, r, session) }))
	t.Cleanup(server.Close)
	ticket, err := service.Issue(context.Background(), identity, session)
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
	ws.SetDeadline(time.Now().Add(10 * time.Second))
	if event := voiceLifecycleRead(t, ws); event.Type != "ready" {
		t.Fatal(event)
	}
	return ws
}
func voiceLifecycleRead(t *testing.T, ws *websocket.Conn) Event {
	t.Helper()
	var event Event
	if err := receiveVoiceResult(ws, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type == "error" {
		t.Fatalf("voice failure: %+v", event)
	}
	return event
}
func voiceLifecycleUntil(t *testing.T, ws *websocket.Conn, kind string) Event {
	t.Helper()
	for {
		event := voiceLifecycleRead(t, ws)
		if event.Type == kind {
			return event
		}
	}
}
func voiceLifecycleShort(t *testing.T, ws *websocket.Conn, segment, recognition string, pcm []byte) (Receipt, Event) {
	t.Helper()
	websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, RecognitionID: recognition, PCM: pcm, Final: true})
	receipt := voiceLifecycleUntil(t, ws, "receipt")
	websocket.JSON.Send(ws, Frame{Type: "recognition_finish", RecognitionID: recognition})
	completed := voiceLifecycleUntil(t, ws, "recognition_completed")
	if completed.RecognitionID != recognition || completed.Milliseconds != (len(pcm)+31)/32 {
		t.Fatalf("invalid ASR completion: %+v", completed)
	}
	return *receipt.Receipt, completed
}
