package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"golang.org/x/net/websocket"
)

type countedContinuousSpeech struct {
	speech Speech
	opens  atomic.Int32
}

func (s *countedContinuousSpeech) Open(ctx context.Context, words []string) (SpeechConnection, error) {
	s.opens.Add(1)
	return s.speech.Open(ctx, words)
}

// This is the real websocket session protocol (audio ACKs + continuous ASR +
// real identity/prose RPCs), not a provider-only probe. App projection, editing,
// persistence and UI remain separate acceptance requirements.
func TestConfiguredContinuousVoiceProtocolAcrossAudioCheckpoints(t *testing.T) {
	path := os.Getenv("JOURNAL_ASR_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("explicit synthetic real-service integration only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private integration configuration")
	}
	var config map[string]string
	if json.Unmarshal(raw, &config) != nil {
		t.Fatal("invalid private integration configuration")
	}
	pcm, err := os.ReadFile(config["PCMPath"])
	if err != nil || len(pcm)%2 != 0 || len(pcm)/32 < 60000 || len(pcm)/32 > SessionMilliseconds {
		t.Fatal("synthetic fixture must cover multiple audio checkpoints")
	}
	if config["JOURNAL_ARK_API_KEY"] == "" || config["JOURNAL_VOICE_MODEL"] == "" {
		t.Fatal("actual configured identity and prose models are required")
	}
	audioMilliseconds := (len(pcm) + 31) / 32
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 200 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	speech := &countedContinuousSpeech{speech: NewBudgetedSpeech(ASR{Config: ASRConfig{URL: "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async", ResourceID: "volc.seedasr.sauc.duration", StreamInsights: true, APIKey: config["JOURNAL_VOICE_ASR_API_KEY"], AppKey: config["JOURNAL_VOICE_ASR_APP_KEY"], AccessKey: config["JOURNAL_VOICE_ASR_ACCESS_KEY"]}}, budget, "journal-development", costcontrol.DurationPrice{NanosPerHour: 4_500_000_000})}
	model := NewBudgetedRewriter(ArkRewriter{BaseURL: config["JOURNAL_ARK_BASE_URL"], APIKey: config["JOURNAL_ARK_API_KEY"], Model: config["JOURNAL_VOICE_MODEL"]}, budget, "journal-development", costcontrol.TokenPrice{InputNanosPerMillionTokens: 2_000_000_000, OutputNanosPerMillionTokens: 12_000_000_000})
	service := &Service{Store: NewMemoryStore(), Speech: speech, Model: model, Secret: make([]byte, 32)}
	session, recognition, block, author := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { service.Serve(w, r, session) }))
	defer server.Close()
	now := time.Now()
	identity := Identity{Owner: "synthetic-continuous-protocol", Anchor: now, ExpiresAt: now.Add(time.Hour)}
	ticket, err := service.Issue(context.Background(), identity, session)
	if err != nil {
		t.Fatal(err)
	}
	socketConfig, _ := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http"), "http://localhost")
	socketConfig.Header.Set("Authorization", "Bearer "+ticket.Token)
	ws, err := websocket.DialConfig(socketConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.SetDeadline(now.Add(time.Duration(len(pcm)/32)*time.Millisecond + 120*time.Second))
	type observation struct {
		ReceivedMilliseconds int64 `json:"receivedMilliseconds"`
		SentMilliseconds     int   `json:"sentMilliseconds"`
		Event                Event `json:"event"`
	}
	trace := []observation{}
	result := map[string]any{"protocolVersion": Version, "appAcceptance": false, "audioMilliseconds": audioMilliseconds, "privateRecordings": false}
	defer func() {
		result["events"] = trace
		result["providerConnections"] = speech.opens.Load()
		data, e := json.MarshalIndent(result, "", "  ")
		if e == nil {
			e = os.WriteFile(config["OutputPath"], data, 0600)
		}
		if e != nil {
			t.Error("could not preserve integration trace")
		}
	}()
	events := make(chan receivedContinuousEvent, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			var event Event
			e := websocket.JSON.Receive(ws, &event)
			select {
			case events <- receivedContinuousEvent{event, e}:
			case <-ctx.Done():
				return
			}
			if e != nil {
				return
			}
		}
	}()
	send := func(frame Frame) {
		t.Helper()
		if e := websocket.JSON.Send(ws, frame); e != nil {
			t.Fatal("continuous protocol send failed", e)
		}
	}
	snapshot := Snapshot{DictationMode: true, WritingStyle: "natural", Blocks: []Block{{ID: block, Text: "原话", Style: "body"}}}
	send(Frame{Type: "snapshot", Snapshot: &snapshot})
	offset, chunkStart, chunkBytes, receipts := 0, 0, 0, 0
	segment := uuid.NewString()
	waiting, recognitionClosing, completed, finishSent := false, false, false, false
	identitySent, identityReceived, polishReceived, multipleDuringCapture := false, false, false, false
	identityDuringCapture, proseDuringCapture := false, false
	var identityTurns []SourceUtterance
	var narratorSpeaker string
	var final Event
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(time.Duration(len(pcm)/32)*time.Millisecond + 110*time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("continuous websocket/AI integration timed out; trace preserved")
		case <-ticker.C:
			if waiting || recognitionClosing || offset == len(pcm) {
				continue
			}
			end := min(offset+6400, len(pcm))
			chunkBytes += end - offset
			finalChunk := chunkBytes >= MaxSegmentBytes || end == len(pcm)
			send(Frame{Type: "audio", RecognitionID: recognition, SegmentID: segment, PCM: pcm[offset:end], Final: finalChunk})
			offset = end
			waiting = finalChunk
		case message := <-events:
			if message.err != nil {
				t.Fatal("continuous protocol receive failed; trace preserved", message.err)
			}
			event := message.event
			trace = append(trace, observation{time.Since(now).Milliseconds(), offset / 32, event})
			switch event.Type {
			case "error", "identity_error", "rewrite_error":
				t.Fatal("actual continuous service failed; trace preserved", event.Type, event.Code)
			case "receipt":
				if event.Receipt == nil || event.Receipt.SegmentID != segment || event.Receipt.SHA256 != hash(string(pcm[chunkStart:offset])) || event.Receipt.Milliseconds != (offset-chunkStart+31)/32 {
					t.Fatal("audio ACK did not match immutable chunk", event)
				}
				receipts++
				t.Logf("audio checkpoint=%d sent_ms=%d provider_connections=%d", receipts, offset/32, speech.opens.Load())
				waiting = false
				chunkBytes = 0
				chunkStart = offset
				segment = uuid.NewString()
				if offset == len(pcm) {
					send(Frame{Type: "capture_closed"})
					send(Frame{Type: "recognition_finish", RecognitionID: recognition})
					recognitionClosing = true
				}
			case "transcript", "recognition_completed":
				if event.RecognitionID != recognition || event.StartMilliseconds != 0 {
					t.Fatal("audio checkpoint replaced recognition scope", event)
				}
				sources := []SourceUtterance{}
				keys := map[string]bool{}
				for _, u := range event.Utterances {
					if u.Definite && u.Speaker != "" {
						keys[u.Speaker] = true
						sources = append(sources, sourceUtterances(recognition, []Utterance{u})...)
					}
				}
				if !recognitionClosing && len(keys) >= 2 {
					multipleDuringCapture = true
				}
				if !identitySent && len(sources) >= 4 && len(keys) >= 2 {
					identityTurns = sources[:min(8, len(sources))]
					narratorSpeaker = identityTurns[0].Speaker
					seen := map[string]bool{}
					speakers := []IdentitySpeaker{}
					for i := range identityTurns {
						if !seen[identityTurns[i].Speaker] {
							value := IdentitySpeaker{Key: identityTurns[i].Speaker}
							if value.Key == narratorSpeaker {
								value.PersonID, value.Name = author, "我"
							}
							speakers = append(speakers, value)
							seen[value.Key] = true
						}
						if identityTurns[i].Speaker == narratorSpeaker {
							identityTurns[i].PersonID = author
						}
					}
					snapshot.Identity = &IdentityRequest{Fingerprint: hash(recognition), Turns: identityTurns, Speakers: speakers, NarratorPersonID: author}
					send(Frame{Type: "snapshot", Snapshot: &snapshot})
					identitySent = true
				}
				if event.Type == "recognition_completed" {
					if !recognitionClosing || event.Milliseconds != audioMilliseconds || len(event.Utterances) < 10 {
						t.Fatal("recognition final lost audio or turns", event)
					}
					final = event
					completed = true
				}
			case "identity":
				if event.Identity == nil || event.Identity.Request.Fingerprint != hash(recognition) || event.Identity.NarratorSourceID != "" {
					t.Fatal("actual identity changed default author or lost request", event)
				}
				names := map[string]string{}
				for _, assignment := range event.Identity.Assignments {
					names[assignment.SpeakerKey] = assignment.Name
				}
				exact := map[string]bool{}
				for _, name := range names {
					exact[name] = true
				}
				if !exact["老婆宝"] || !exact["老公宝"] {
					t.Fatal("actual contextual identity did not retain complete names", event.Identity)
				}
				identityReceived = true
				t.Logf("identity received_ms=%d while_capture=%t", time.Since(now).Milliseconds(), offset < len(pcm))
				identityDuringCapture = !completed && offset < len(pcm)
				result["identity"] = event.Identity
				snapshot.Identity = nil
				body, ids := "", []string{}
				for i := range identityTurns {
					value := &identityTurns[i]
					value.Person = names[value.Speaker]
					value.PersonID = uuid.NewString()
					if value.Speaker == narratorSpeaker {
						value.PersonID = author
					}
					body += value.Text
					ids = append(ids, value.ID)
				}
				narrator := &Narrator{PersonID: author, Name: names[narratorSpeaker]}
				snapshot.Narrator = narrator
				snapshot.Blocks[0].Text = body
				snapshot.Polish = &PolishRequest{Narrator: narrator, Context: []Block{}, Targets: []PolishTarget{{Style: "body", ID: block, Text: body, SourceText: body, SourceIDs: ids, Turns: identityTurns, CompleteSource: true}}}
				send(Frame{Type: "snapshot", Snapshot: &snapshot})
			case "polish":
				if event.Polish == nil || len(event.Polish.Paragraphs) == 0 {
					t.Fatal("actual AI returned no prose")
				}
				body := ""
				for _, paragraph := range event.Polish.Paragraphs {
					body += paragraph.Text
				}
				if !strings.Contains(body, "老婆宝") || !strings.Contains(body, "医院") {
					t.Fatal("actual prose lost other-speaker attribution; trace preserved")
				}
				polishReceived = true
				t.Logf("prose received_ms=%d while_capture=%t", time.Since(now).Milliseconds(), offset < len(pcm))
				proseDuringCapture = !completed && offset < len(pcm)
				result["polish"] = event.Polish
				result["body"] = body
				snapshot.Blocks[0].Text = body
				snapshot.AcknowledgedPolishID = event.Polish.ID
				snapshot.Polish.Targets = []PolishTarget{}
				send(Frame{Type: "snapshot", Snapshot: &snapshot})
			case "finished":
				result["audioReceiptCount"], result["multipleVoicesDuringCapture"], result["identityDuringCapture"], result["proseDuringCapture"] = receipts, multipleDuringCapture, identityDuringCapture, proseDuringCapture
				result["recognitionCompleted"] = final
				if !completed || !multipleDuringCapture || !identityReceived || !polishReceived || !identityDuringCapture || !proseDuringCapture || receipts < 4 || speech.opens.Load() != 1 {
					t.Fatal("continuous protocol milestones missing; trace preserved")
				}
				for _, marker := range strings.Split(config["ExpectedMarkers"], ",") {
					if marker != "" && !strings.Contains(final.Text, marker) {
						t.Fatalf("actual ASR lost fixture marker %q; trace preserved", marker)
					}
				}
				remaining, e := service.Store.Remaining(context.Background(), identity.Owner, func() string { start, _ := Period(identity.Anchor, time.Now()); return start.Format(time.RFC3339) }(), service.limit())
				if e != nil || remaining != service.limit()-audioMilliseconds {
					t.Fatal("independent audio quota mismatch", remaining, e)
				}
				result["passed"] = true
				return
			}
			if completed && identityReceived && polishReceived && !finishSent {
				send(Frame{Type: "finish"})
				finishSent = true
			}
		}
	}
}

type receivedContinuousEvent struct {
	event Event
	err   error
}
