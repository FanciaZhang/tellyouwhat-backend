package development

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
)

// Explicitly enabled, synthetic-only acceptance. The cumulative ledger is
// supplied by the operator; restarting this test never resets its 50 CNY cap.
func TestConfiguredReadAloudTiming(t *testing.T) {
	key, directory := os.Getenv("JOURNAL_SYNTHETIC_READ_ALOUD_KEY_FILE"), os.Getenv("JOURNAL_SYNTHETIC_READ_ALOUD_OUTPUT")
	if key == "" || directory == "" {
		t.Skip("explicit configured-provider acceptance only")
	}
	ledgerPath := filepath.Join(directory, "cost-events.json")
	if _, err := os.Stat(ledgerPath); err != nil {
		t.Fatal("existing cumulative ledger required")
	}
	ledger, err := NewFileCostStore(ledgerPath, 50*costcontrol.NanosPerCNY, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	budget, err := costcontrol.New(ledger, costcontrol.Limits{MonthlyBudgetNanos: 50 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := readaloud.NewProviderFromFile(key)
	if err != nil {
		t.Fatal(err)
	}
	metered, err := readaloud.NewMeteredSynthesizer(provider, budget, readaloud.StandardCharacterNanos)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct{ name, text, dialect string }{
		{"long", strings.Repeat("沿着河岸慢慢往前走阳光从树叶的缝隙里落下来今天不需要赶路只想把这些安静的片刻认真记下来", 5), ""},
		{"dialect", strings.Repeat("今天沿着河边走了一会儿，慢慢记下路上的风景。", 3), "beijing"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var pcm []byte
			var cues []readaloud.Timing
			input := readaloud.Synthesis{RequestID: uuid.NewString(), Text: sample.text, VoiceID: "zh_female_xiaohe_uranus_bigtts", Style: "natural", Dialect: sample.dialect,
				OnTimings: func(values []readaloud.Timing) error { cues = append([]readaloud.Timing(nil), values...); return nil }}
			usage, callErr := metered.Synthesize(t.Context(), input, func(data []byte) error { pcm = append(pcm, data...); return nil })
			receipt := struct {
				Synthetic bool
				Text      string
				Timings   []readaloud.Timing
				Usage     readaloud.SynthesisUsage
				Success   bool
			}{true, sample.text, cues, usage, callErr == nil}
			data, _ := json.MarshalIndent(receipt, "", "  ")
			if err := os.WriteFile(filepath.Join(directory, sample.name+".json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if len(pcm) > 0 {
				if err := os.WriteFile(filepath.Join(directory, sample.name+".pcm"), pcm, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if callErr != nil {
				t.Fatal(callErr)
			}
			if len(cues) < 2 || usage.AudioBytes != len(pcm) || !usage.UsageKnown ||
				cues[len(cues)-1].UTF16Location+cues[len(cues)-1].UTF16Length != len(utf16.Encode([]rune(sample.text))) ||
				cues[len(cues)-1].EndMilliseconds > len(pcm)*1000/(readaloud.SampleRate*2)+100 {
				t.Fatalf("playable audio lacks verified text positions: cues=%d bytes=%d", len(cues), len(pcm))
			}
			t.Logf("configured synthesis: bytes=%d cues=%d characters=%d", len(pcm), len(cues), usage.Characters)
		})
	}
}
