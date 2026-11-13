package voice

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

// Opt-in loopback acceptance gives the App a bounded operator credential for
// real providers, independently of Apple purchases and production accounts.
func TestConfiguredImportedAudioAppHarness(t *testing.T) {
	path := os.Getenv("JOURNAL_IMPORT_APP_ACCEPTANCE_CONFIG")
	if path == "" {
		t.Skip("explicit synthetic App and real-provider acceptance only")
	}
	var config struct {
		Provider                   map[string]string
		Token, ReadyPath, StopPath string
		MaxCostNanos               int64
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &config) != nil || len(config.Token) < 32 ||
		!filepath.IsAbs(config.ReadyPath) || !filepath.IsAbs(config.StopPath) ||
		config.MaxCostNanos <= 0 || config.MaxCostNanos > costcontrol.NanosPerCNY {
		t.Fatal("private bounded synthetic-acceptance configuration required")
	}
	p := config.Provider
	if p["JOURNAL_ARK_API_KEY"] == "" || p["JOURNAL_VOICE_MODEL"] == "" ||
		(p["JOURNAL_VOICE_ASR_API_KEY"] == "" && (p["JOURNAL_VOICE_ASR_APP_KEY"] == "" || p["JOURNAL_VOICE_ASR_ACCESS_KEY"] == "")) {
		t.Fatal("actual configured speech and narrative providers required")
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{
		MonthlyBudgetNanos: config.MaxCostNanos, MaxConcurrent: 2, LeaseDuration: 5 * time.Minute,
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		Store: NewMemoryStore(), Secret: secret,
		Speech: NewBudgetedSpeech(ASR{Config: ASRConfig{
			URL: p["JOURNAL_VOICE_ASR_URL"], ResourceID: p["JOURNAL_VOICE_ASR_RESOURCE_ID"], StreamInsights: true,
			APIKey: p["JOURNAL_VOICE_ASR_API_KEY"], AppKey: p["JOURNAL_VOICE_ASR_APP_KEY"], AccessKey: p["JOURNAL_VOICE_ASR_ACCESS_KEY"],
		}}, budget, "journal-import-acceptance", costcontrol.DurationPrice{NanosPerHour: 4_500_000_000}),
		Model: NewBudgetedRewriter(ArkRewriter{BaseURL: p["JOURNAL_ARK_BASE_URL"], APIKey: p["JOURNAL_ARK_API_KEY"], Model: p["JOURNAL_VOICE_MODEL"]},
			budget, "journal-import-acceptance", costcontrol.TokenPrice{InputNanosPerMillionTokens: 9_600_000_000, OutputNanosPerMillionTokens: 48_000_000_000}),
	}
	owner := "synthetic-import-" + uuid.NewString()
	proof := sha256.Sum256([]byte("Bearer " + config.Token))
	var tickets atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/journal/voice/sessions", func(w http.ResponseWriter, r *http.Request) {
		provided := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		var body struct {
			SessionID string `json:"sessionID"`
		}
		if subtle.ConstantTimeCompare(proof[:], provided[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body) != nil || uuid.Validate(body.SessionID) != nil || tickets.Add(1) > 3 {
			http.Error(w, "invalid acceptance session", http.StatusBadRequest)
			return
		}
		now := time.Now()
		ticket, err := service.Issue(r.Context(), Identity{Owner: owner, Anchor: now, ExpiresAt: now.Add(time.Hour)}, body.SessionID)
		if err != nil {
			http.Error(w, "ticket unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ticket)
	})
	mux.HandleFunc("GET /v1/journal/voice/sessions/{session}/stream", func(w http.ResponseWriter, r *http.Request) {
		service.Serve(w, r, r.PathValue("session"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	ready, _ := json.Marshal(map[string]any{"baseURL": server.URL, "budgetLimitNanos": config.MaxCostNanos, "realProviders": true})
	if err := os.WriteFile(config.ReadyPath, ready, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("App acceptance did not finish within five minutes")
		case <-ticker.C:
			if _, err := os.Stat(config.StopPath); err == nil {
				t.Logf("real-provider App acceptance issued %d tickets", tickets.Load())
				return
			}
		}
	}
}
