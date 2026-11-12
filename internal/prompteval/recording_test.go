package prompteval

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/voice"
)

func TestRecordingSampleRejectsUntrustedManifests(t *testing.T) {
	id := uuid.NewString()
	manifest := RecordingInput{Chunks: []string{id}, Bytes: 32000, SHA256: strings.Repeat("a", 64)}
	sample := Sample{ID: uuid.NewString(), Name: "Synthetic recording", Kind: "voice_recording", VoiceDebug: true, Recording: &manifest}
	if err := sample.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Sample){
		func(s *Sample) { s.Kind = "voice_transcription"; s.Voice = &voice.Snapshot{}; s.Audio = []byte{0, 0} },
		func(s *Sample) { s.Recording.SHA256 = strings.Repeat("z", 64) },
		func(s *Sample) { s.Recording.Chunks = []string{id, id} },
		func(s *Sample) { s.Recording.Bytes = 32001 },
		func(s *Sample) { s.VoiceDebug = false },
	} {
		value, input := sample, manifest
		value.Recording = &input
		mutate(&value)
		if !errors.Is(value.Validate(), ErrInvalid) {
			t.Fatal("invalid recording manifest accepted", value)
		}
	}
}

type recordingFixtureProvider struct {
	calls, queries int
	audio          []byte
	missing        bool
	invalid        bool
	cancel         context.CancelFunc
}

func (p *recordingFixtureProvider) SubmitPCM(_ context.Context, _ string, input io.Reader, size int) error {
	p.calls++
	data, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	if size != len(data) {
		return ErrInvalid
	}
	p.audio = data
	return nil
}
func (p *recordingFixtureProvider) Query(_ context.Context, id string, ms int) (voice.RecordingAnalysis, error) {
	p.queries++
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	if p.missing {
		return voice.RecordingAnalysis{}, voice.ErrRecordingTaskMissing
	}
	if p.invalid {
		return voice.RecordingAnalysis{TaskID: uuid.NewString(), Milliseconds: ms}, nil
	}
	return voice.RecordingAnalysis{TaskID: id, Version: voice.RecordingAnalysisVersion, Milliseconds: ms, Text: "合成录音原文。", Utterances: []voice.RecordingUtterance{{ID: uuid.NewString(), Text: "合成录音原文。", StartMilliseconds: 0, EndMilliseconds: ms, Speaker: "0"}}}, nil
}
func TestRecordingInputOwnershipFrozenBytesReconcileAndRetention(t *testing.T) {
	s, m := testStore(t)
	ctx := context.Background()
	now := time.Now()
	var ids []string
	var pcm []byte
	for i := 0; i < 2; i++ {
		part := Sample{ID: uuid.NewString(), Name: "合成音频分片", Kind: "voice_transcription", VoiceDebug: true, Voice: &voice.Snapshot{}, Audio: bytes.Repeat([]byte{byte(i + 1), 7}, 16000), Expected: []Check{}}
		if err := s.SaveSample(ctx, part, m.Actor, now); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, part.ID)
		pcm = append(pcm, part.Audio...)
	}
	if _, err := s.RecordingInput(ctx, ids, uuid.NewString(), now); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign sample readable", err)
	}
	if _, err := s.RecordingInput(ctx, []string{ids[0], ids[0]}, m.Actor, now); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate chunk", err)
	}
	manifest, err := s.RecordingInput(ctx, ids, m.Actor, now)
	if err != nil {
		t.Fatal(err)
	}
	sample := Sample{ID: uuid.NewString(), Name: "完整合成录音", Kind: "voice_recording", VoiceDebug: true, Recording: &manifest, Expected: []Check{}}
	price := costcontrol.DurationPrice{NanosPerHour: 4500000000}
	plan, err := PrepareVoiceDebug([]Sample{sample}, testPlan(t).Candidates[0], price)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.Start(ctx, plan, m, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetForActor(ctx, run.ID, uuid.NewString(), now); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign run", err)
	}
	rows, err := s.ListForActor(ctx, uuid.NewString(), now)
	if err != nil || len(rows) != 0 {
		t.Fatal("foreign history", err)
	}
	var encrypted []byte
	if err = s.DB.QueryRow(`SELECT payload FROM prompt_eval_recording_inputs WHERE run_id=? AND part_index=0`, run.ID).Scan(&encrypted); err != nil || bytes.Contains(encrypted, pcm[:100]) {
		t.Fatal("input not encrypted", err)
	}
	for _, id := range ids {
		if err = s.DeleteSample(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	provider := &recordingFixtureProvider{missing: true}
	engine := Engine{Store: s, SpeechPrice: price, RecordingSpeech: provider}
	claim, err := s.Claim(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	attempt, cancel := context.WithCancel(ctx)
	provider.cancel = cancel
	engine.Process(attempt, claim)
	cancel()
	item, err := s.Result(ctx, run.ID, 0, time.Now())
	if err != nil || item.Result.Outputs[0].Error != "cancelled" {
		t.Fatal("uncertain submission not retained", err, item)
	}
	originalID := item.Result.Outputs[0].RecordingTaskID
	if provider.calls != 1 || !bytes.Equal(provider.audio, pcm) {
		t.Fatal("frozen input changed", provider.calls, len(provider.audio))
	}
	if err = s.ReconcileRecording(ctx, run.ID, 0, uuid.NewString(), now); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign reconcile", err)
	}
	provider.missing = false
	if err = s.ReconcileRecording(ctx, run.ID, 0, m.Actor, now); err != nil {
		t.Fatal(err)
	}
	claim, err = s.Claim(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	engine.Process(ctx, claim)
	item, err = s.Result(ctx, run.ID, 0, time.Now())
	if err != nil || item.Status != "completed" || item.Result.Outputs[0].RecordingTaskID != originalID || provider.calls != 1 {
		t.Fatal("reconcile repeated submit", err, item, provider.calls)
	}
	if item.Result.Outputs[0].Text != "合成录音原文。" {
		t.Fatal("text lost")
	}
	if err = s.Recover(ctx, now.Add(Retention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM prompt_eval_recording_inputs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired input survived run cleanup", err, count)
	}
}
