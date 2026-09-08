// journaldevserver is a isolated, persistent Journal development service.
// It is deliberately absent from the production image build targets.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/development"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/journal/voice"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// The fixed loopback listener is reached through the authenticated Journal HTTPS route.
	if os.Getenv("DATABASE_DSN") != "" || os.Getenv("REDIS_URL") != "" || os.Getenv("APP_ENV") == "production" {
		return errors.New("refusing production configuration in developer service")
	}
	base, key := os.Getenv("JOURNAL_ARK_BASE_URL"), os.Getenv("JOURNAL_ARK_API_KEY")
	lite, pro, rewrite := os.Getenv("JOURNAL_ARK_LITE_MODEL"), os.Getenv("JOURNAL_ARK_PRO_MODEL"), os.Getenv("JOURNAL_VOICE_MODEL")
	asr := voice.ASRConfig{URL: os.Getenv("JOURNAL_VOICE_ASR_URL"), APIKey: os.Getenv("JOURNAL_VOICE_ASR_API_KEY"), AppKey: os.Getenv("JOURNAL_VOICE_ASR_APP_KEY"), AccessKey: os.Getenv("JOURNAL_VOICE_ASR_ACCESS_KEY"), ResourceID: os.Getenv("JOURNAL_VOICE_ASR_RESOURCE_ID")}
	if base == "" || key == "" || lite == "" || pro == "" || rewrite == "" || asr.URL == "" || asr.ResourceID == "" || (asr.APIKey == "" && (asr.AppKey == "" || asr.AccessKey == "")) {
		return errors.New("missing Journal provider configuration")
	}
	stateDir := os.Getenv("JOURNAL_DEVELOPMENT_STATE_DIR")
	if stateDir == "" {
		return errors.New("missing persistent development state directory")
	}
	monthlyCNY, err := strconv.ParseInt(os.Getenv("JOURNAL_DEVELOPMENT_MONTHLY_BUDGET_CNY"), 10, 64)
	if err != nil || monthlyCNY < 1 || monthlyCNY > 1000 {
		return errors.New("invalid development monthly budget")
	}
	store, err := development.NewFileCostStore(filepath.Join(stateDir, "cost-events.json"))
	if err != nil {
		return err
	}
	budget, err := costcontrol.New(store, costcontrol.Limits{MonthlyBudgetNanos: monthlyCNY * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		return err
	}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 9_600_000_000, OutputNanosPerMillionTokens: 48_000_000_000}
	model := provider.New(provider.Config{BaseURL: base, APIKey: key, LiteModel: lite, ProModel: pro}, &http.Client{Timeout: 2 * time.Minute})
	handler, err := development.New(development.Config{
		Token:     os.Getenv("JOURNAL_DEVELOPMENT_TOKEN"),
		Organizer: provider.NewBudgetedClient(model, budget, "journal-development", price),
		Speech:    voice.NewBudgetedSpeech(voice.ASR{Config: asr}, budget, "journal-development", costcontrol.DurationPrice{NanosPerHour: 4_500_000_000}),
		Rewriter:  voice.NewBudgetedRewriter(voice.ArkRewriter{BaseURL: base, APIKey: key, Model: rewrite}, budget, "journal-development", price),
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: "127.0.0.1:18787", Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: nil}
	go func() { <-ctx.Done(); _ = server.Close() }()
	fmt.Println("Journal development ready; credential valid until revoked; audio 120 minutes/month")
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
