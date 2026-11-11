package prompteval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func debugPolishSample() Sample {
	s := FinalizationSample()
	s.ID, s.VoiceDebug = uuid.NewString(), true
	return s
}

func TestVoiceDebugReservesActualProductionCallsWithoutJudge(t *testing.T) {
	candidate := testPlan(t).Candidates[0]
	candidate.Revision.Policy.Journal.Voice.PolishPrompt = "保留散步的具体细节。"
	price := costcontrol.DurationPrice{NanosPerHour: 4500000000}
	sample := debugPolishSample()
	plan, err := PrepareVoiceDebug([]Sample{sample}, candidate, price)
	if err != nil || plan.Mode != VoiceDebugMode || plan.MaximumCalls != 1 || len(plan.Requests) != 1 {
		t.Fatal(plan, err)
	}
	prepared, err := voice.PrepareRewrite(promptconfig.WithRevision(context.Background(), candidate.Revision), *sample.Voice, 1, "")
	want, _ := prepared.Parameters.Price.Cost(len(prepared.Body)+1024, prepared.Parameters.MaxOutputTokens)
	if err != nil || !bytes.Equal(prepared.Body, plan.Requests[0]) || plan.ReservedNanos != want {
		t.Fatal("preview/cost diverged from app finalization", err)
	}
	transcription := Sample{ID: uuid.NewString(), Name: "Synthetic audio", Kind: "voice_transcription", VoiceDebug: true, Voice: &voice.Snapshot{Words: []string{"小林"}}, Audio: make([]byte, 32000)}
	asr, err := PrepareVoiceDebug([]Sample{transcription}, candidate, price)
	want, _ = price.Cost(15000)
	if err != nil || asr.MaximumCalls != 1 || asr.ReservedNanos != want || !bytes.Contains(asr.Requests[0], []byte(`"milliseconds":1000`)) {
		t.Fatal("speech reservation must cover the production admission window", err)
	}
	if _, err := Prepare([]Sample{transcription}, []Candidate{candidate}, candidate.Revision.Policy.Journal.Organize.Pro, price); err == nil {
		t.Fatal("ASR-only sample accepted in publication evaluation")
	}
	for _, bad := range []Sample{
		{ID: uuid.NewString(), Name: "empty audio", Kind: "voice_transcription", VoiceDebug: true, Voice: &voice.Snapshot{}},
		{ID: uuid.NewString(), Name: "oversized audio", Kind: "voice_transcription", VoiceDebug: true, Voice: &voice.Snapshot{}, Audio: make([]byte, voice.MaxSegmentBytes+2)},
		{ID: uuid.NewString(), Name: "unaligned audio", Kind: "voice_transcription", VoiceDebug: true, Voice: &voice.Snapshot{}, Audio: []byte{0}},
		Builtins()[0],
	} {
		if _, err := PrepareVoiceDebug([]Sample{bad}, candidate, price); err == nil {
			t.Fatal("invalid quick sample accepted", bad.Name)
		}
	}
}

func TestVoiceDebugExecutesValidatedPolishAndKeepsFailures(t *testing.T) {
	sample := debugPolishSample()
	candidate := testPlan(t).Candidates[0]
	candidate.Revision.Policy.Journal.Voice.PolishPrompt = "本次调试保留地点。"
	bad := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if !strings.Contains(request["instructions"].(string), candidate.Revision.Policy.Journal.Voice.PolishPrompt) {
			t.Error("finalization prompt not sent")
		}
		value, _ := json.Marshal(map[string]any{"paragraphs": []voice.PolishParagraph{{TargetIDs: []string{sample.Voice.Polish.Targets[0].ID}, Text: sample.Voice.Transcript, Style: "body"}}, "omissions": []any{}, "questions": []any{}})
		if bad {
			value = []byte(`{"paragraphs":[]}`)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "model": "fixture-finalizer", "usage": map[string]int{"input_tokens": 20, "output_tokens": 30}, "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(value)}}}}})
	}))
	defer server.Close()
	budget, _ := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 100e9, MaxConcurrent: 2, LeaseDuration: time.Minute}, time.Now)
	engine := Engine{Provider: provider.Config{BaseURL: server.URL, APIKey: "fixture"}, Price: *candidate.Revision.Policy.Journal.Voice.Parameters.Price}
	ctx := promptconfig.WithRevision(context.Background(), candidate.Revision)
	out := engine.execute(ctx, budget, sample, candidate, 0)
	if out.Error != "" || !strings.Contains(out.Text, sample.Voice.Transcript) || !bytes.Contains(out.Structured, []byte("paragraphs")) || out.Model != "fixture-finalizer" || out.Transcript != sample.Voice.Transcript {
		t.Fatalf("polish lost or applied as old patch: %+v", out)
	}
	bad = true
	out = engine.execute(ctx, budget, sample, candidate, 0)
	if out.Error == "" || out.Text != "" || out.DiagnosticOutput == "" || out.Diagnostics == nil || out.Transcript != sample.Voice.Transcript {
		t.Fatalf("invalid output disguised as journal or diagnostics lost: %+v", out)
	}
}

type debugSpeech struct{ fail bool }
type debugConnection struct{ final, closed bool }

func (s debugSpeech) Open(ctx context.Context, words []string) (voice.SpeechConnection, error) {
	if s.fail {
		return nil, errors.New("synthetic provider failure")
	}
	return &debugConnection{}, nil
}
func (c *debugConnection) Send(_ []byte, final bool) error { c.final = final; return nil }
func (c *debugConnection) Receive() (voice.Transcript, error) {
	return voice.Transcript{Text: "今天在河边散步。", Final: c.final}, nil
}
func (c *debugConnection) Close() error { c.closed = true; return nil }

func TestMySQLVoiceDebugBudgetQueueRecoveryAndGateIsolation(t *testing.T) {
	s, mutation := testStore(t)
	candidate := testPlan(t).Candidates[0]
	price := costcontrol.DurationPrice{NanosPerHour: 4500000000}
	sample := Sample{ID: uuid.NewString(), Name: "Synthetic one-second ASR", Kind: "voice_transcription", VoiceDebug: true, Voice: &voice.Snapshot{}, Audio: make([]byte, 32000)}
	plan, err := PrepareVoiceDebug([]Sample{sample}, candidate, price)
	if err != nil {
		t.Fatal(err)
	}
	ctx, now := context.Background(), time.Now()
	if err = s.SaveSample(ctx, sample, mutation.Actor, now); err != nil {
		t.Fatal(err)
	}
	library, err := s.Samples(ctx, now)
	if err != nil || len(library) != 0 {
		t.Fatal("quick audio samples leaked into publication library", err)
	}
	run, err := s.Start(ctx, plan, mutation, now)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Start(ctx, plan, mutation, now)
	if err != nil || replay.ID != run.ID {
		t.Fatal("paid command not idempotent", err)
	}
	if _, err = s.Command(ctx, uuid.NewString(), mutation.Key, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("command lookup escaped its actor", err)
	}
	claim, err := s.Claim(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	engine := Engine{Store: s, Speech: debugSpeech{}, SpeechPrice: price}
	engine.Process(ctx, claim)
	item, err := s.Result(ctx, run.ID, 0, time.Now())
	if err != nil || item.Status != "completed" || item.Result.Judgment.Status != "not_applicable" || item.Result.Outputs[0].Text != "今天在河边散步。" {
		t.Fatalf("ASR did not run independently: %+v %v", item, err)
	}
	var remaining int64
	if err = s.DB.QueryRow(`SELECT remaining_nanos FROM prompt_eval_budget_holds WHERE run_id=?`, run.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("budget hold leaked", remaining, err)
	}
	var revisions string
	if err = s.DB.QueryRow(`SELECT candidate_revisions FROM prompt_eval_runs WHERE id=?`, run.ID).Scan(&revisions); err != nil || strings.Contains(revisions, `"`+candidate.Revision.ID+`"`) {
		t.Fatal("quick debug can shadow production evaluation", revisions, err)
	}
	blockers, err := s.PublicationBlockers(ctx, candidate.Revision, now)
	if err != nil || len(blockers) == 0 {
		t.Fatal("debug run certified publication", err)
	}
	if err = s.Recover(ctx, now.Add(Retention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(ctx, run.ID, now.Add(Retention+time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired audio/results not removed", err)
	}
}
