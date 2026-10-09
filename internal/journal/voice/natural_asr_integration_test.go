package voice

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

// A separate provider probe proves that a continuous connection can expose
// completed turns before final input. App chunk/upload behavior is validated
// independently; success here must not be reported as App acceptance.
func TestConfiguredNaturalASRReturnsCompletedTurnsDuringCapture(t *testing.T) {
	testConfiguredNaturalASR(t, 0)
}

func TestConfiguredContinuousASRAndIdentityAcrossBudgetWindows(t *testing.T) {
	testConfiguredNaturalASR(t, 60_000)
}

func testConfiguredNaturalASR(t *testing.T, minimumMilliseconds int) {
	t.Helper()
	path := os.Getenv("JOURNAL_ASR_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("Explicit real configured ASR integration only")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private ASR configuration")
	}
	var config map[string]string
	if json.Unmarshal(b, &config) != nil {
		t.Fatal("invalid ASR configuration")
	}
	pcm, err := os.ReadFile(config["PCMPath"])
	if err != nil {
		t.Fatal("read synthetic PCM")
	}
	if len(pcm)%2 != 0 || len(pcm)/32 < minimumMilliseconds || len(pcm)/32 > SessionMilliseconds {
		t.Fatal("synthetic audio does not cover the requested continuous duration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(pcm)/32)*time.Millisecond+60*time.Second)
	defer cancel()
	a := ASR{Config: ASRConfig{URL: "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async",
		ResourceID: "volc.seedasr.sauc.duration", StreamInsights: true,
		APIKey: config["JOURNAL_VOICE_ASR_API_KEY"], AppKey: config["JOURNAL_VOICE_ASR_APP_KEY"], AccessKey: config["JOURNAL_VOICE_ASR_ACCESS_KEY"]}}
	var speech Speech = a
	var budget *costcontrol.Controller
	if minimumMilliseconds > 0 {
		if config["JOURNAL_ARK_API_KEY"] == "" || config["JOURNAL_ARK_BASE_URL"] == "" || config["JOURNAL_VOICE_MODEL"] == "" {
			t.Fatal("continuous integration also requires the actual configured AI")
		}
		budget, err = costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 200 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		speech = NewBudgetedSpeech(a, budget, "journal-development", costcontrol.DurationPrice{NanosPerHour: 4_500_000_000})
	}
	c, err := speech.Open(ctx, nil)
	if err != nil {
		t.Fatal("configured ASR open failed", err)
	}
	defer c.Close()
	type record struct {
		ReceivedAt       int64
		SentMilliseconds int64
		Result           Transcript
	}
	started := time.Now()
	var sent atomic.Int64
	var finalSent atomic.Bool
	var duringTurns atomic.Bool
	type completion struct {
		err     error
		records []record
	}
	done := make(chan completion, 1)
	go func() {
		records := []record{}
		for {
			v, e := c.Receive()
			if e != nil {
				done <- completion{e, records}
				return
			}
			records = append(records, record{time.Since(started).Milliseconds(), sent.Load(), v})
			if !finalSent.Load() {
				keys := map[string]bool{}
				for _, u := range v.Utterances {
					if u.Definite && u.Speaker != "" {
						keys[u.Speaker] = true
					}
				}
				if len(keys) >= 2 {
					duringTurns.Store(true)
				}
			}
			if v.Final {
				done <- completion{nil, records}
				return
			}
		}
	}()
	var sendError error
	for offset := 0; offset < len(pcm); offset += 6400 {
		end := min(offset+6400, len(pcm))
		last := end == len(pcm)
		if last {
			finalSent.Store(true)
		}
		if err := c.Send(pcm[offset:end], last); err != nil {
			sendError = err
			_ = c.Close()
			break
		}
		sent.Store(int64(end / 32))
		time.Sleep(200 * time.Millisecond)
	}
	var completed completion
	select {
	case completed = <-done:
	case <-time.After(35 * time.Second):
		_ = c.Close()
		select {
		case completed = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("configured ASR did not finish or close")
		}
	}
	result := map[string]any{"records": completed.records, "multipleCompletedVoicesBeforeFinalInput": duringTurns.Load(), "audioMilliseconds": len(pcm) / 32,
		"budgetWindowsEnabled": minimumMilliseconds > 0, "appAcceptance": false, "disfluencyCleanupEnabled": false}
	out, _ := json.MarshalIndent(result, "", "  ")
	if e := os.WriteFile(config["OutputPath"], out, 0600); e != nil {
		t.Fatal(e)
	}
	if sendError != nil || completed.err != nil {
		t.Fatal("configured ASR failed; response trace preserved", sendError, completed.err)
	}
	if !duringTurns.Load() {
		t.Fatal("continuous ASR did not identify multiple completed voices before final input; response trace preserved")
	}
	if minimumMilliseconds == 0 {
		return
	}
	if len(completed.records) == 0 {
		t.Fatal("continuous ASR returned no results")
	}
	final := completed.records[len(completed.records)-1].Result
	if !final.Final || len(final.Utterances) < 10 || final.Utterances[0].StartMilliseconds > 3000 || final.Utterances[len(final.Utterances)-1].EndMilliseconds < minimumMilliseconds {
		t.Fatal("continuous ASR lost earlier or later source turns; trace preserved")
	}
	for _, marker := range strings.Split(config["ExpectedMarkers"], ",") {
		if marker != "" && !strings.Contains(final.Text, marker) {
			t.Fatalf("actual ASR dropped fixture content %q; trace preserved", marker)
		}
	}
	// One recognition connection scopes all speaker IDs, independent of its
	// rolling billing checkpoints. No manual naming is provided to the model.
	scope := uuid.NewString()
	sources := sourceUtterances(scope, identifiedUtterances(scope, final.Text, incrementalUtterances(final.Utterances, len(pcm)/32), len(pcm)/32))
	if len(sources) < 10 {
		t.Fatal("canonical conversion flattened the continuous source evidence before actual AI")
	}
	first := sources[0].Speaker
	personID := uuid.NewString()
	seen := map[string]bool{}
	speakers := []IdentitySpeaker{}
	for i := range sources {
		if !seen[sources[i].Speaker] {
			s := IdentitySpeaker{Key: sources[i].Speaker}
			if s.Key == first {
				s.PersonID, s.Name = personID, "我"
			}
			speakers = append(speakers, s)
			seen[s.Key] = true
		}
		if sources[i].Speaker == first {
			sources[i].PersonID = personID
		}
	}
	request := &IdentityRequest{Fingerprint: hash(scope), Turns: sources[max(0, len(sources)-24):], Speakers: speakers, NarratorSpeaker: first}
	if len(speakers) < 2 {
		t.Fatal("canonical conversion erased the actual ASR speaker scopes")
	}
	result["identityRequest"] = request
	model := NewBudgetedRewriter(ArkRewriter{BaseURL: config["JOURNAL_ARK_BASE_URL"], APIKey: config["JOURNAL_ARK_API_KEY"], Model: config["JOURNAL_VOICE_MODEL"]}, budget,
		"journal-development", costcontrol.TokenPrice{InputNanosPerMillionTokens: 2_000_000_000, OutputNanosPerMillionTokens: 12_000_000_000})
	identity, modelErr := model.Rewrite(ctx, Snapshot{Identity: request, DictationMode: true, WritingStyle: "natural", Blocks: []Block{{ID: uuid.NewString(), Text: "原话", Style: "body"}}}, 1)
	result["identity"] = identity.Identity
	result["model"] = identity.Model
	result["inputTokens"], result["outputTokens"] = identity.InputTokens, identity.OutputTokens
	result["identitySucceeded"] = modelErr == nil
	out, _ = json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(config["OutputPath"], out, 0600); err != nil {
		t.Fatal(err)
	}
	if modelErr != nil || identity.Identity == nil {
		t.Fatal("configured identity AI failed after continuous ASR; trace preserved", modelErr)
	}
	names := map[string]bool{}
	for _, a := range identity.Identity.Assignments {
		names[a.Name] = true
	}
	if !names["老婆宝"] || !names["老公宝"] || identity.Identity.NarratorSpeaker != "" {
		t.Fatal("continuous ASR identity did not preserve reciprocal names and default author; trace preserved")
	}
}
