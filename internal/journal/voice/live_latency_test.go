package voice

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"golang.org/x/net/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// Explicit live diagnostic, never enabled in ordinary tests. Input must be a
// synthetic PCM16 mono 16kHz fixture, not a user's recording. Logs timings only.
func TestLiveStreamingLatency(t *testing.T) {
	if os.Getenv("JOURNAL_VOICE_LATENCY_CHECK") != "1" {
		t.Skip("explicit synthetic latency acceptance only")
	}
	pcm, err := os.ReadFile(os.Getenv("JOURNAL_VOICE_LIVE_PCM"))
	if err != nil || len(pcm) < 32000 || len(pcm) > 60*32000 || len(pcm)%2 != 0 {
		t.Fatal("synthetic PCM must be 1–60 seconds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	provider := ASR{Config: ASRConfig{URL: "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async", APIKey: os.Getenv("JOURNAL_SPEECH_API_KEY"), AppKey: os.Getenv("JOURNAL_SPEECH_APP_KEY"), AccessKey: os.Getenv("JOURNAL_SPEECH_ACCESS_KEY"), ResourceID: "volc.seedasr.sauc.duration"}}
	opened := time.Now()
	connection, err := provider.Open(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	t.Logf("connection_ms=%d audio_ms=%d", time.Since(opened).Milliseconds(), len(pcm)/32)
	started := time.Now()
	type measured struct {
		first, final int64
		lags         []int64
		chars        int
		err          error
	}
	done := make(chan measured, 1)
	go func() {
		m := measured{}
		lastEnd := int64(0)
		for {
			c := connection.(*asrConnection)
			c.ws.SetReadDeadline(time.Now().Add(15 * time.Second))
			var packet []byte
			if err := websocket.Message.Receive(c.ws, &packet); err != nil {
				m.err = err
				done <- m
				return
			}
			tr, err := parseASR(packet)
			if err != nil {
				m.err = err
				done <- m
				return
			}
			elapsed := time.Since(started).Milliseconds()
			if tr.Text != "" && m.first == 0 {
				m.first = elapsed
			}
			offset := int(packet[0]&15) * 4
			if packet[1]&1 != 0 {
				offset += 4
			}
			payload := packet[offset+4:]
			if packet[2]&15 == 1 {
				reader, _ := gzip.NewReader(bytes.NewReader(payload))
				payload, _ = io.ReadAll(reader)
				reader.Close()
			}
			var envelope struct {
				Result struct {
					Utterances []struct {
						End int64 `json:"end_time"`
					} `json:"utterances"`
				} `json:"result"`
			}
			_ = json.Unmarshal(payload, &envelope)
			end := int64(0)
			for _, u := range envelope.Result.Utterances {
				if u.End > end {
					end = u.End
				}
			}
			t.Logf("progress elapsed_ms=%d audio_end_ms=%d text_chars=%d stable_chars=%d", elapsed, end, len([]rune(tr.Text)), len([]rune(tr.Stable)))
			if end > lastEnd {
				m.lags = append(m.lags, elapsed-end)
				lastEnd = end
			}
			m.chars = len([]rune(tr.Text))
			if tr.Final {
				m.final = elapsed
				if !strings.Contains(tr.Text, "公园") {
					m.err = ErrInvalid
				}
				done <- m
				return
			}
		}
	}()
	for offset := 0; offset < len(pcm); offset += 6400 {
		end := min(offset+6400, len(pcm))
		// A microphone cannot deliver a packet until that audio has been captured.
		due := started.Add(time.Duration(end) * time.Second / 32000)
		if delay := time.Until(due); delay > 0 {
			time.Sleep(delay)
		}
		if err := connection.Send(pcm[offset:end], end == len(pcm)); err != nil {
			t.Fatal(err)
		}
	}
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	sort.Slice(result.lags, func(i, j int) bool { return result.lags[i] < result.lags[j] })
	if len(result.lags) == 0 {
		t.Fatal("no intermediate timestamped progress")
	}
	t.Logf("first_text_ms=%d final_tail_ms=%d progress_samples=%d recognition_lag_p50_ms=%d recognition_lag_p95_ms=%d transcript_chars=%d", result.first, result.final-int64(len(pcm)/32), len(result.lags), result.lags[len(result.lags)/2], result.lags[(len(result.lags)-1)*95/100], result.chars)
	if result.first >= int64(len(pcm)/32) {
		t.Fatal("no transcription until recording ended")
	}
}

// Exercise the complete candidate session with actual providers, without
// deploying it or touching shared production services/entitlements.
func TestLiveStreamingDiaryLatency(t *testing.T) {
	if os.Getenv("JOURNAL_VOICE_LATENCY_CHECK") != "1" {
		t.Skip("explicit synthetic latency acceptance only")
	}
	pcm, err := os.ReadFile(os.Getenv("JOURNAL_VOICE_LIVE_PCM"))
	if err != nil || len(pcm) == 0 || len(pcm) > 60*32000 {
		t.Fatal("synthetic fixture required")
	}
	model := os.Getenv("JOURNAL_VOICE_MODEL_ID")
	if model == "" {
		model = os.Getenv("JOURNAL_ARK_PRO_MODEL_ID")
	}
	// Include the real cost/concurrency wrappers; a direct provider-only check
	// cannot detect a local budget gate blocking otherwise healthy providers.
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{
		MonthlyBudgetNanos: 100 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute,
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	speech := ASR{Config: ASRConfig{URL: "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async", APIKey: os.Getenv("JOURNAL_SPEECH_API_KEY"), ResourceID: "volc.seedasr.sauc.duration"}}
	editor := ArkRewriter{BaseURL: os.Getenv("JOURNAL_ARK_BASE_URL"), APIKey: os.Getenv("JOURNAL_ARK_API_KEY"), Model: model}
	service := &Service{Store: NewMemoryStore(), Secret: make([]byte, 32),
		Speech: NewBudgetedSpeech(speech, budget, "journal-development", costcontrol.DurationPrice{NanosPerHour: 4_500_000_000}),
		Model:  NewBudgetedRewriter(editor, budget, "journal-development", costcontrol.TokenPrice{InputNanosPerMillionTokens: 9_600_000_000, OutputNanosPerMillionTokens: 48_000_000_000}),
	}
	session := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { service.Serve(w, r, session) }))
	defer server.Close()
	ticket, err := service.Issue(context.Background(), Identity{Owner: "synthetic-latency", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, session)
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
	ws.SetDeadline(time.Now().Add(120 * time.Second))
	type message struct {
		event Event
		err   error
	}
	events := make(chan message, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			var e Event
			err := websocket.JSON.Receive(ws, &e)
			select {
			case events <- message{e, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	snapshot := Snapshot{Blocks: []Block{{ID: uuid.NewString(), Text: ""}}}
	send := func(frame Frame) {
		t.Helper()
		if err := websocket.JSON.Send(ws, frame); err != nil {
			t.Fatal(err)
		}
	}
	send(Frame{Type: "snapshot", Snapshot: &snapshot})
	started := time.Now()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(110 * time.Second)
	defer deadline.Stop()
	offset, segmentBytes, revisions := 0, 0, 0
	segment := uuid.NewString()
	waiting, finished := false, false
	var firstText, firstBody int64
	for {
		select {
		case <-deadline.C:
			t.Fatal("streaming diary timed out")
		case <-ticker.C:
			if waiting || finished {
				continue
			}
			if offset == len(pcm) {
				send(Frame{Type: "finish"})
				finished = true
				continue
			}
			end := min(offset+6400, len(pcm))
			if time.Since(started) < time.Duration(end)*time.Second/32000 {
				continue
			}
			segmentBytes += end - offset
			final := segmentBytes >= MaxSegmentBytes || end == len(pcm)
			send(Frame{Type: "audio", SegmentID: segment, PCM: pcm[offset:end], Final: final})
			offset = end
			waiting = final
		case message := <-events:
			if message.err != nil {
				t.Fatal(message.err)
			}
			e := message.event
			switch e.Type {
			case "error":
				t.Fatal(e.Code)
			case "transcript":
				if e.Text != "" && firstText == 0 {
					firstText = time.Since(started).Milliseconds()
				}
			case "receipt":
				snapshot.Transcript += e.Receipt.Text
				send(Frame{Type: "snapshot", Snapshot: &snapshot})
				waiting = false
				segmentBytes = 0
				segment = uuid.NewString()
			case "revision":
				for _, p := range e.Revision.Patches {
					found := false
					for i := range snapshot.Blocks {
						if snapshot.Blocks[i].ID == p.ID {
							snapshot.Blocks[i].Text = p.Text
							found = true
							break
						}
					}
					if !found {
						at := len(snapshot.Blocks)
						for i, b := range snapshot.Blocks {
							if b.ID == p.AfterID {
								at = i + 1
								break
							}
						}
						snapshot.Blocks = append(snapshot.Blocks, Block{})
						copy(snapshot.Blocks[at+1:], snapshot.Blocks[at:])
						snapshot.Blocks[at] = Block{ID: p.ID, Text: p.Text}
					}
				}
				snapshot.Revision++
				send(Frame{Type: "snapshot", Snapshot: &snapshot})
				revisions++
				if firstBody == 0 {
					firstBody = time.Since(started).Milliseconds()
				}
				t.Logf("rewrite elapsed_ms=%d revision=%d body_chars=%d", time.Since(started).Milliseconds(), revisions, len([]rune(bodyText(snapshot))))
			case "finished":
				if !strings.Contains(bodyText(snapshot), "公园") || !strings.Contains(bodyText(snapshot), "茶") {
					t.Fatal("final diary lost synthetic facts")
				}
				t.Logf("diary first_text_ms=%d first_body_ms=%d final_tail_ms=%d revisions=%d", firstText, firstBody, time.Since(started).Milliseconds()-int64(len(pcm)/32), revisions)
				if firstBody >= int64(len(pcm)/32) {
					t.Fatal("no live diary update while speaking")
				}
				return
			}
		}
	}
}
func bodyText(s Snapshot) string {
	var b strings.Builder
	for _, block := range s.Blocks {
		b.WriteString(block.Text)
	}
	return b.String()
}
