// journaldevserver is a private, short-lived Journal development service.
// It is deliberately absent from the production image build targets.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
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
	// A fixed loopback listener cannot accidentally become a public bypass.
	if os.Getenv("DATABASE_DSN") != "" || os.Getenv("REDIS_URL") != "" || os.Getenv("APP_ENV") == "production" {
		return errors.New("refusing production configuration in developer service")
	}
	expires, err := time.Parse(time.RFC3339, os.Getenv("JOURNAL_DEVELOPMENT_EXPIRES_AT"))
	if err != nil {
		return errors.New("missing developer session expiry")
	}
	base, key := os.Getenv("JOURNAL_ARK_BASE_URL"), os.Getenv("JOURNAL_ARK_API_KEY")
	lite, pro, rewrite := os.Getenv("JOURNAL_ARK_LITE_MODEL"), os.Getenv("JOURNAL_ARK_PRO_MODEL"), os.Getenv("JOURNAL_VOICE_MODEL")
	asr := voice.ASRConfig{URL: os.Getenv("JOURNAL_VOICE_ASR_URL"), APIKey: os.Getenv("JOURNAL_VOICE_ASR_API_KEY"), AppKey: os.Getenv("JOURNAL_VOICE_ASR_APP_KEY"), AccessKey: os.Getenv("JOURNAL_VOICE_ASR_ACCESS_KEY"), ResourceID: os.Getenv("JOURNAL_VOICE_ASR_RESOURCE_ID")}
	if base == "" || key == "" || lite == "" || pro == "" || rewrite == "" || asr.URL == "" || asr.ResourceID == "" || (asr.APIKey == "" && (asr.AppKey == "" || asr.AccessKey == "")) {
		return errors.New("missing Journal provider configuration")
	}
	// Conservative server-owned prices match the shared gateway's upper bounds.
	// Pin the accounting month for this session so crossing midnight/month-end
	// cannot reset the single-session budget. Only an operator can start a new one.
	started := time.Now()
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Since(started)) })
	if err != nil {
		return err
	}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 9_600_000_000, OutputNanosPerMillionTokens: 48_000_000_000}
	model := provider.New(provider.Config{BaseURL: base, APIKey: key, LiteModel: lite, ProModel: pro}, &http.Client{Timeout: 2 * time.Minute})
	handler, err := development.New(development.Config{
		Token: os.Getenv("JOURNAL_DEVELOPMENT_TOKEN"), ExpiresAt: expires,
		Organizer: provider.NewBudgetedClient(model, budget, "journal-development", price),
		Speech:    voice.NewBudgetedSpeech(voice.ASR{Config: asr}, budget, "journal-development", costcontrol.DurationPrice{NanosPerHour: 4_500_000_000}),
		Rewriter:  voice.NewBudgetedRewriter(voice.ArkRewriter{BaseURL: base, APIKey: key, Model: rewrite}, budget, "journal-development", price),
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	server := &http.Server{Addr: "127.0.0.1:18787", Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: nil}
	go func() { <-ctx.Done(); _ = server.Close() }()
	fmt.Println("Journal development ready on loopback; expires", expires.Format(time.RFC3339), "; budget CNY 1; audio 10 minutes")
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
