package prompteval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

// Opt in with a private provider-settings file and an explicit remaining budget.
// The preview is free. Execution uses synthetic speech, the real providers and
// the same encrypted durable queue as the admin workbench, in an isolated DB.
func TestLiveVoiceDebug(t *testing.T) {
	file := os.Getenv("JOURNAL_VOICE_DEBUG_LIVE_CONFIG")
	if file == "" {
		t.Skip("explicit real-provider check only")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("private provider configuration unavailable")
	}
	var settings map[string]string
	if json.Unmarshal(raw, &settings) != nil {
		t.Fatal("invalid provider configuration")
	}
	maximum, err := strconv.ParseInt(os.Getenv("JOURNAL_VOICE_DEBUG_MAX_NANOS"), 10, 64)
	if err != nil || maximum <= 0 {
		t.Fatal("explicit remaining budget required")
	}
	model := settings["JOURNAL_VOICE_MODEL_ID"]
	if model == "" {
		model = settings["JOURNAL_ARK_PRO_MODEL_ID"]
	}
	policy := promptconfig.Defaults(settings["JOURNAL_ARK_LITE_MODEL_ID"], settings["JOURNAL_ARK_PRO_MODEL_ID"], model, 90)["journal"]
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 9600000000, OutputNanosPerMillionTokens: 48000000000}
	speechPrice := costcontrol.DurationPrice{NanosPerHour: 4500000000}
	policy.Journal.Voice.Parameters.Price = &price
	policy.Journal.Voice.Parameters.MaxOutputTokens = 512
	policy.Journal.Voice.PolishPrompt = "理顺句子、按事件分段；保留散步、热茶等原文细节。"
	candidate := Candidate{Revision: promptconfig.Revision{ID: uuid.NewString(), Scope: "journal", Policy: policy}, Label: "Synthetic live workbench"}
	audio, err := os.ReadFile("fixtures/river.pcm")
	if err != nil {
		t.Fatal(err)
	}
	asr := Sample{ID: uuid.NewString(), Name: "Synthetic river speech", Kind: "voice_transcription", VoiceDebug: true, Voice: &voice.Snapshot{}, Audio: audio}
	asrPlan, err := PrepareVoiceDebug([]Sample{asr}, candidate, speechPrice)
	if err != nil {
		t.Fatal(err)
	}
	polish := debugPolishSample()
	polishPlan, err := PrepareVoiceDebug([]Sample{polish}, candidate, speechPrice)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("FREE PREVIEW: ASR reserve=%.6f CNY; finalizer example reserve=%.6f CNY; remaining limit=%.6f CNY", float64(asrPlan.ReservedNanos)/1e9, float64(polishPlan.ReservedNanos)/1e9, float64(maximum)/1e9)
	if os.Getenv("JOURNAL_VOICE_DEBUG_LIVE_EXECUTE") != "1" {
		return
	}
	if asrPlan.ReservedNanos+polishPlan.ReservedNanos > maximum {
		t.Fatal("preview exceeds the remaining authorized budget; no provider request sent")
	}
	directory := os.Getenv("JOURNAL_VOICE_DEBUG_EVIDENCE_DIR")
	if directory == "" {
		t.Fatal("persistent evidence directory required")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, mutation := testStore(t)
	store.Limits.MonthlyBudgetNanos = maximum
	engine := Engine{Store: store, Provider: provider.Config{BaseURL: settings["JOURNAL_ARK_BASE_URL"], APIKey: settings["JOURNAL_ARK_API_KEY"]}, ASR: voice.ASRConfig{URL: "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async", APIKey: settings["JOURNAL_SPEECH_API_KEY"], AppKey: settings["JOURNAL_SPEECH_APP_KEY"], AccessKey: settings["JOURNAL_SPEECH_ACCESS_KEY"], ResourceID: "volc.seedasr.sauc.duration", StreamInsights: true}, Price: price, SpeechPrice: speechPrice}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	outputs := []Output{}
	defer func() {
		var accounted int64
		err := store.DB.QueryRow(`SELECT COALESCE(SUM(IF(status='settled',actual_nanos,reserved_nanos)),0) FROM ai_cost_attempts`).Scan(&accounted)
		if err != nil {
			t.Error("cost reconciliation unavailable", err)
		}
		raw, _ := json.MarshalIndent(map[string]any{"synthetic": true, "outputs": outputs, "accountedNanos": accounted, "remainingLimitNanos": maximum, "passed": !t.Failed()}, "", "  ")
		if err := os.WriteFile(filepath.Join(directory, "live-voice-debug.json"), raw, 0600); err != nil {
			t.Error(err)
		}
		t.Logf("accounted or conservatively reserved cost %.6f CNY", float64(accounted)/1e9)
	}()
	runSample := func(sample Sample) Output {
		t.Helper()
		if err := store.SaveSample(ctx, sample, mutation.Actor, time.Now()); err != nil {
			t.Fatal(err)
		}
		plan, err := PrepareVoiceDebug([]Sample{sample}, candidate, speechPrice)
		if err != nil {
			t.Fatal(err)
		}
		mutation.Key = uuid.NewString()
		run, err := store.Start(ctx, plan, mutation, time.Now())
		if err != nil {
			t.Fatal("budget admission failed before provider call", err)
		}
		claim, err := store.Claim(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		engine.Process(ctx, claim)
		item, err := store.Result(ctx, run.ID, 0, time.Now())
		if err != nil || item.Result == nil || len(item.Result.Outputs) != 1 {
			t.Fatal("durable result unavailable", err)
		}
		out := item.Result.Outputs[0]
		outputs = append(outputs, out)
		if out.Error != "" || item.Status != "completed" || item.Result.Judgment.Status != "not_applicable" {
			t.Fatalf("real stage failed: %s, status %s", out.Error, item.Status)
		}
		return out
	}
	transcript := runSample(asr)
	if !strings.Contains(transcript.Text, "河边") || !strings.Contains(transcript.Text, "热茶") {
		t.Fatal("synthetic speech facts missing; evidence preserved")
	}
	polish.Voice.Transcript = transcript.Text
	polish.Voice.Blocks[0].Text = transcript.Text
	polish.Voice.Polish.Targets[0].Text = transcript.Text
	polish.Voice.Polish.Targets[0].SourceText = transcript.Text
	polish.Voice.Polish.Targets[0].Turns[0].Text = transcript.Text
	finalized := runSample(polish)
	if finalized.Text == "" || !strings.Contains(finalized.Text, "河边") || !strings.Contains(finalized.Text, "热茶") {
		t.Fatal("final journal did not retain source facts")
	}
	t.Logf("real ASR and finalization persisted and reloaded: model=%s, tokens=%d/%d", finalized.Model, finalized.InputTokens, finalized.OutputTokens)
}
