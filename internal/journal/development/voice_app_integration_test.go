package development

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

type syntheticVoicePolicy struct{ revision promptconfig.Revision }

func (p syntheticVoicePolicy) Published(context.Context) (map[string]promptconfig.Revision, error) {
	return map[string]promptconfig.Revision{"journal": p.revision}, nil
}

// An opt-in, loopback-only candidate for synthetic App acceptance. It opens no
// production storage or Apple accounts and exits after its owner signals stop.
func TestConfiguredSyntheticVoiceAppServer(t *testing.T) {
	path := os.Getenv("JOURNAL_SYNTHETIC_VOICE_APP_CONFIG")
	if path == "" {
		t.Skip("explicit synthetic App integration only")
	}
	var c struct {
		BaseURL, APIKey, Lite, Pro, Voice, Token, OutputDirectory string
		ASR                                                       voice.ASRConfig
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &c) != nil || c.APIKey == "" || len(c.Token) < 43 || c.Lite == "" || c.Pro == "" || c.Voice == "" || c.OutputDirectory == "" {
		t.Fatal("private candidate configuration required")
	}
	if err := os.MkdirAll(c.OutputDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 10_000_000_000, OutputNanosPerMillionTokens: 100_000_000_000}
	policy := promptconfig.Defaults(c.Lite, c.Pro, c.Voice, 90)["journal"]
	policy.Journal.Voice.Parameters.Price = &price
	policy.Journal.Organize.Lite.Price = &price
	published := time.Now()
	prompts := promptconfig.NewJournalCache(syntheticVoicePolicy{promptconfig.Revision{ID: "synthetic-voice-candidate", Scope: "journal", Policy: policy, PublishedAt: &published}})
	if err := prompts.Refresh(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	ledger, err := NewFileCostStore(filepath.Join(c.OutputDirectory, "cost-events.json"), 5*costcontrol.NanosPerCNY, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	budget, err := costcontrol.New(ledger, costcontrol.Limits{MonthlyBudgetNanos: 5 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("synthetic", true)
	model := provider.New(provider.Config{BaseURL: c.BaseURL, APIKey: c.APIKey, LiteModel: c.Lite, ProModel: c.Pro}, &http.Client{Timeout: 2 * time.Minute})
	handler, err := New(Config{Token: c.Token, Prompts: prompts, Logger: logger,
		Organizer: provider.NewBudgetedClient(model, budget, "journal-synthetic-acceptance", price),
		Speech:    voice.NewBudgetedSpeech(voice.ASR{Config: c.ASR, Prompts: prompts}, budget, "journal-synthetic-acceptance", costcontrol.DurationPrice{NanosPerHour: 4_500_000_000}),
		Rewriter:  voice.ConfiguredRewriter{Next: voice.NewBudgetedRewriter(voice.ArkRewriter{BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.Voice, Logger: logger}, budget, "journal-synthetic-acceptance", price), Config: prompts},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ready, _ := json.Marshal(map[string]any{"url": server.URL, "voiceProtocol": voice.Version, "budgetCNY": 5, "synthetic": true})
	if err := os.WriteFile(filepath.Join(c.OutputDirectory, "ready.json"), ready, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(75 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.Context().Done():
			t.Fatal("candidate interrupted")
		case <-deadline.C:
			t.Fatal("candidate acceptance deadline exceeded")
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(c.OutputDirectory, "stop")); err == nil {
				return
			}
		}
	}
}
