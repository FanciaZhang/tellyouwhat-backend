package voice

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Explicit operator acceptance only. The fixture is synthetic PCM16 mono 16kHz,
// saying: 今天下午我和小明去公园散步。不是小明，是小林。后来我们在湖边喝了一杯茶。
// Never point this check at a user's recording or run it in ordinary unit tests.
func TestLiveSpeechAndDiaryRewrite(t *testing.T) {
	if os.Getenv("JOURNAL_VOICE_LIVE_CHECK") != "1" {
		t.Skip("live provider acceptance must be explicitly enabled")
	}
	pcm, err := os.ReadFile(os.Getenv("JOURNAL_VOICE_LIVE_PCM"))
	if err != nil || len(pcm) == 0 || len(pcm) > MaxSegmentBytes || len(pcm)%2 != 0 {
		t.Fatal("provide the documented synthetic PCM fixture, at most 15 seconds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	asr := ASR{Config: ASRConfig{
		URL:    "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async",
		APIKey: os.Getenv("JOURNAL_SPEECH_API_KEY"), AppKey: os.Getenv("JOURNAL_SPEECH_APP_KEY"),
		AccessKey: os.Getenv("JOURNAL_SPEECH_ACCESS_KEY"), ResourceID: "volc.seedasr.sauc.duration",
	}}
	conn, err := asr.Open(ctx, []string{"小林"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	type result struct {
		transcript Transcript
		frames     int
		err        error
	}
	results := make(chan result, 1)
	go func() {
		for frames := 1; ; frames++ {
			transcript, err := conn.Receive()
			if err != nil || transcript.Final {
				results <- result{transcript, frames, err}
				return
			}
		}
	}()
	for offset := 0; offset < len(pcm); offset += 6400 {
		end := min(offset+6400, len(pcm))
		if err := conn.Send(pcm[offset:end], false); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := conn.Send(nil, true); err != nil {
		t.Fatal(err)
	}
	var speech result
	select {
	case speech = <-results:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if speech.err != nil {
		t.Fatal(speech.err)
	}
	if !speech.transcript.Final || !strings.Contains(speech.transcript.Text, "公园") {
		t.Fatal("speech provider did not finish recognizing the synthetic park fixture")
	}
	baseURL := os.Getenv("JOURNAL_ARK_BASE_URL")
	if baseURL == "" {
		baseURL = "https://ark.cn-beijing.volces.com/api/v3"
	}
	model := os.Getenv("JOURNAL_VOICE_MODEL_ID")
	if model == "" {
		model = os.Getenv("JOURNAL_ARK_PRO_MODEL_ID")
	}
	rewriter := ArkRewriter{BaseURL: baseURL, APIKey: os.Getenv("JOURNAL_ARK_API_KEY"), Model: model}
	rewritten, err := rewriter.Rewrite(ctx, Snapshot{
		Revision: 0, Blocks: []Block{{ID: uuid.NewString(), Text: ""}},
		Transcript: speech.transcript.Text, Words: []string{"小林"},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	var manuscript string
	for _, patch := range rewritten.Revision.Patches {
		manuscript += patch.Text
	}
	if !strings.Contains(manuscript, "公园") || !strings.Contains(manuscript, "小林") || !strings.Contains(manuscript, "茶") {
		t.Fatal("rewritten synthetic diary lost a stated person, place or activity")
	}
	t.Logf("live synthetic fixture passed: %d speech frames, %d transcript runes, %d patches, tokens %d/%d",
		speech.frames, len([]rune(speech.transcript.Text)), len(rewritten.Revision.Patches), rewritten.InputTokens, rewritten.OutputTokens)
}
