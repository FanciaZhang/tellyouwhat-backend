package voice

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Opt-in only. Supply synthetic or dedicated consented test audio, never a
// user's journal. Each invocation submits exactly one billable provider task.
func TestLiveRecordingAnalysis(t *testing.T) {
	if os.Getenv("JOURNAL_LIVE_RECORDING") != "1" {
		t.Skip("explicit live recording opt-in required")
	}
	path := os.Getenv("JOURNAL_LIVE_RECORDING_WAV")
	audio, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	milliseconds := (len(audio) - 44) / 32
	a := RecordingASR{Config: ASRConfig{ResourceID: "volc.seedasr.auc", APIKey: os.Getenv("JOURNAL_VOICE_ASR_API_KEY"), AppKey: os.Getenv("JOURNAL_VOICE_ASR_APP_KEY"), AccessKey: os.Getenv("JOURNAL_VOICE_ASR_ACCESS_KEY")}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	id := uuid.NewString()
	start := time.Now()
	t.Logf("task=%s milliseconds=%d", id, milliseconds)
	if err := a.Submit(ctx, id, audio); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(3 * time.Second):
		}
		result, err := a.Query(ctx, id, milliseconds)
		if err == ErrRecordingPending {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result)
		t.Logf("elapsed=%s result=%s", time.Since(start), encoded)
		return
	}
}
