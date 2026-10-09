package voice

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

type continuousTestSpeech struct {
	mu                        sync.Mutex
	opens, bytes, finalInputs int
	conn                      *scriptedConnection
	finalInput                chan struct{}
	failReceive               bool
}

func (s *continuousTestSpeech) Open(context.Context, []string) (SpeechConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	return &continuousTestConnection{owner: s}, nil
}

type continuousTestConnection struct{ owner *continuousTestSpeech }

func (c *continuousTestConnection) Send(pcm []byte, final bool) error {
	s := c.owner
	s.mu.Lock()
	s.bytes += len(pcm)
	if final {
		s.finalInputs++
		close(s.finalInput)
	}
	s.mu.Unlock()
	return nil
}
func (c *continuousTestConnection) Receive() (Transcript, error) {
	if c.owner.failReceive {
		<-c.owner.conn.closed
		return Transcript{}, errors.New("synthetic recognition failure")
	}
	return c.owner.conn.Receive()
}
func (c *continuousTestConnection) Close() error { return c.owner.conn.Close() }
func (s *continuousTestSpeech) counts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens, s.bytes, s.finalInputs
}
func newContinuousTestSpeech() *continuousTestSpeech {
	return &continuousTestSpeech{conn: &scriptedConnection{result: make(chan Transcript, 8), closed: make(chan struct{})}, finalInput: make(chan struct{})}
}

func TestAudioCheckpointsKeepOneRecognitionAndContinuousTurnTimes(t *testing.T) {
	speech := newContinuousTestSpeech()
	service := &Service{Store: NewMemoryStore(), Speech: speech, Secret: make([]byte, 32)}
	ws := voiceLifecycleSocket(t, service, Identity{Owner: "continuous-checkpoints", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, uuid.NewString())
	snapshot := Snapshot{DictationMode: true, Blocks: []Block{{uuid.NewString(), "", ""}}}
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	recognition := uuid.NewString()
	for range 2 {
		segment := uuid.NewString()
		for i := 0; i < MaxSegmentBytes/6400; i++ {
			if err := websocket.JSON.Send(ws, Frame{Type: "audio", SegmentID: segment, RecognitionID: recognition, StartMilliseconds: 5000, PCM: make([]byte, 6400), Final: i == MaxSegmentBytes/6400-1}); err != nil {
				t.Fatal(err)
			}
		}
		event := voiceLifecycleUntil(t, ws, "receipt")
		if event.Receipt.Milliseconds != 15000 {
			t.Fatal(event)
		}
		opens, bytes, finals := speech.counts()
		if opens != 1 || finals != 0 || bytes%MaxSegmentBytes != 0 {
			t.Fatal("audio checkpoint ended ASR", opens, bytes, finals)
		}
	}
	turns := []StreamUtterance{{Text: "我和老婆宝散步。", StartMilliseconds: 1000, EndMilliseconds: 18000, Definite: true, Speaker: "0"}, {Text: "我跟老公宝很开心。", StartMilliseconds: 19000, EndMilliseconds: 29000, Definite: true, Speaker: "1"}}
	speech.conn.result <- Transcript{Text: turns[0].Text + turns[1].Text, Utterances: turns}
	event := voiceLifecycleRead(t, ws)
	if event.Type != "transcript" || event.RecognitionID != recognition || event.StartMilliseconds != 5000 || event.Milliseconds != 30000 || len(event.Utterances) != 2 || event.Utterances[1].EndMilliseconds != 29000 {
		t.Fatal("audio-boundary clamp/flatten broke turn", event)
	}
	websocket.JSON.Send(ws, Frame{Type: "recognition_finish", RecognitionID: recognition})
	select {
	case <-speech.finalInput:
	case <-time.After(time.Second):
		t.Fatal("ASR input did not close")
	}
	speech.conn.result <- Transcript{Text: event.Text, Utterances: turns, Final: true}
	completed := voiceLifecycleRead(t, ws)
	if completed.Type != "recognition_completed" || completed.Utterances[0].ID != event.Utterances[0].ID || completed.Milliseconds != 30000 {
		t.Fatal("ASR final lost continuous source", completed)
	}
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	if event := voiceLifecycleRead(t, ws); event.Type != "finished" {
		t.Fatal(event)
	}
}

func TestAudioReceiptCannotAuthorizeEditorialFinishBeforeRecognitionTail(t *testing.T) {
	speech := newContinuousTestSpeech()
	service := &Service{Store: NewMemoryStore(), Speech: speech, Secret: make([]byte, 32)}
	ws := voiceLifecycleSocket(t, service, Identity{Owner: "unfinished-tail", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, uuid.NewString())
	websocket.JSON.Send(ws, Frame{Type: "audio", RecognitionID: uuid.NewString(), SegmentID: uuid.NewString(), PCM: make([]byte, 6400), Final: true})
	voiceLifecycleUntil(t, ws, "receipt")
	websocket.JSON.Send(ws, Frame{Type: "finish"})
	var event Event
	if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "error" || event.Code != "voice_invalid_request" {
		t.Fatal("silently completed unfinished ASR", event, err)
	}
}

func TestBilledReplayValidatesWholeChunkBeforeProviderReceivesAnyByte(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "identical"}[valid], func(t *testing.T) {
			store := NewMemoryStore()
			session, segment := uuid.NewString(), uuid.NewString()
			owner := "verified-replay"
			now := time.Now()
			period, _ := Period(now, now)
			pcm := make([]byte, 12800)
			store.Lock(context.Background(), owner, "seed")
			store.Commit(context.Background(), owner, session, period.Format(time.RFC3339), "seed", Receipt{SegmentID: segment, SHA256: hash(string(pcm)), Milliseconds: 400}, 400)
			store.Unlock(context.Background(), owner, "seed")
			speech := newContinuousTestSpeech()
			service := &Service{Store: store, Speech: speech, Secret: make([]byte, 32), Limit: 400}
			ws := voiceLifecycleSocket(t, service, Identity{Owner: owner, Anchor: now, ExpiresAt: now.Add(time.Hour)}, session)
			recognition := uuid.NewString()
			websocket.JSON.Send(ws, Frame{Type: "audio", RecognitionID: recognition, SegmentID: segment, PCM: pcm[:6400]})
			websocket.JSON.Send(ws, Frame{Type: "ping"})
			voiceLifecycleUntil(t, ws, "pong")
			opens, bytes, _ := speech.counts()
			if opens != 0 || bytes != 0 {
				t.Fatal("unverified billed prefix reached provider", opens, bytes)
			}
			if !valid {
				pcm[12799] = 1
			}
			websocket.JSON.Send(ws, Frame{Type: "audio", RecognitionID: recognition, SegmentID: segment, PCM: pcm[6400:], Final: true})
			var event Event
			if err := receiveVoiceResult(ws, &event); err != nil {
				t.Fatal(err)
			}
			opens, bytes, _ = speech.counts()
			if valid {
				if event.Type != "receipt" || opens != 1 || bytes != 12800 {
					t.Fatal(event, opens, bytes)
				}
			} else {
				if event.Type != "error" || event.Code != "voice_revision_conflict" || opens != 0 || bytes != 0 {
					t.Fatal("old bill authorized changed audio", event, opens, bytes)
				}
			}
		})
	}
}

func TestFailedRecognitionKeepsIndependentAudioAcknowledgement(t *testing.T) {
	speech := newContinuousTestSpeech()
	speech.failReceive = true
	store := NewMemoryStore()
	session, segment := uuid.NewString(), uuid.NewString()
	now := time.Now()
	owner := "independent-recovery"
	service := &Service{Store: store, Speech: speech, Secret: make([]byte, 32)}
	ws := voiceLifecycleSocket(t, service, Identity{Owner: owner, Anchor: now, ExpiresAt: now.Add(time.Hour)}, session)
	websocket.JSON.Send(ws, Frame{Type: "audio", RecognitionID: uuid.NewString(), SegmentID: segment, PCM: make([]byte, 6400), Final: true})
	voiceLifecycleUntil(t, ws, "receipt")
	speech.conn.Close()
	var event Event
	err := receiveVoiceResult(ws, &event)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if event.Type != "error" || event.Code != "voice_speech_unavailable" {
		t.Fatal(event, err)
	}
	receipt, err := store.Receipt(context.Background(), owner, session, segment)
	if err != nil || receipt == nil || receipt.Milliseconds != 200 {
		t.Fatal("recognition failure erased audio acknowledgement", receipt, err)
	}
}
