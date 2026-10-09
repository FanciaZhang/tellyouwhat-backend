package voice

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// A separate provider probe proves that a continuous connection can expose
// completed turns before final input. App chunk/upload behavior is validated
// independently; success here must not be reported as App acceptance.
func TestConfiguredNaturalASRReturnsCompletedTurnsDuringCapture(t *testing.T) {
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
	a := ASR{Config: ASRConfig{URL: "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async",
		ResourceID: "volc.seedasr.sauc.duration", StreamInsights: true,
		APIKey: config["JOURNAL_VOICE_ASR_API_KEY"], AppKey: config["JOURNAL_VOICE_ASR_APP_KEY"], AccessKey: config["JOURNAL_VOICE_ASR_ACCESS_KEY"]}}
	c, err := a.Open(context.Background(), nil)
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
	done := make(chan error, 1)
	records := []record{}
	go func() {
		for {
			v, e := c.Receive()
			if e != nil {
				done <- e
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
				done <- nil
				return
			}
		}
	}()
	for offset := 0; offset < len(pcm); offset += 6400 {
		end := min(offset+6400, len(pcm))
		last := end == len(pcm)
		if last {
			finalSent.Store(true)
		}
		if err := c.Send(pcm[offset:end], last); err != nil {
			t.Fatal("configured ASR send failed", err)
		}
		sent.Store(int64(end / 32))
		time.Sleep(200 * time.Millisecond)
	}
	select {
	case err = <-done:
	case <-time.After(35 * time.Second):
		t.Fatal("configured ASR did not finish")
	}
	out, _ := json.MarshalIndent(map[string]any{"records": records, "multipleCompletedVoicesBeforeFinalInput": duringTurns.Load(), "audioMilliseconds": len(pcm) / 32}, "", "  ")
	if e := os.WriteFile(config["OutputPath"], out, 0600); e != nil {
		t.Fatal(e)
	}
	if err != nil {
		t.Fatal("configured ASR receive failed", err)
	}
	if !duringTurns.Load() {
		t.Fatal("continuous ASR did not identify multiple completed voices before final input; response trace preserved")
	}
}
