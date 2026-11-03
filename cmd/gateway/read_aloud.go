package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/gateway"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
	"github.com/tellyouwhat/backend/internal/journal/simulatortest"
	"github.com/tellyouwhat/backend/internal/privacy"
)

type readAloudConfig struct {
	keyFile          string
	journal, preview int
}

func loadReadAloudConfig() (*readAloudConfig, error) {
	flag := os.Getenv("JOURNAL_TTS_ENABLED")
	if flag == "" || flag == "false" {
		return nil, nil
	}
	if flag != "true" {
		return nil, errors.New("invalid Journal TTS enable flag")
	}
	c := &readAloudConfig{keyFile: os.Getenv("JOURNAL_TTS_API_KEY_FILE"), preview: 1000}
	var err error
	c.journal, err = strconv.Atoi(os.Getenv("JOURNAL_TTS_MONTHLY_CHARACTERS"))
	if err != nil || c.journal < 1 || c.journal > 100_000_000 || c.keyFile == "" {
		return nil, errors.New("Journal TTS requires a private key file and bounded monthly allowance")
	}
	if raw := os.Getenv("JOURNAL_TTS_PREVIEW_DAILY_CHARACTERS"); raw != "" {
		c.preview, err = strconv.Atoi(raw)
		if err != nil || c.preview < 1 || c.preview > 1_000_000 {
			return nil, errors.New("invalid Journal TTS preview allowance")
		}
	}
	return c, nil
}
func configuredReadAloud(ctx context.Context, c *readAloudConfig, storage appStorage, costs *costcontrol.Controller, consents *privacy.Service, simulator *simulatortest.Service, entitlements readaloud.EntitlementReader, development bool) (*readaloud.SessionService, gateway.ReadAloudAccess, error) {
	if c == nil {
		return nil, nil, nil
	}
	if storage.voiceCatalog == nil || storage.voiceCatalog.DB == nil || storage.background == nil || costs == nil {
		return nil, nil, errors.New("Journal TTS requires durable storage, managed workers and project cost protection")
	}
	provider, err := readaloud.NewProviderFromFile(c.keyFile)
	if err != nil {
		return nil, nil, err
	}
	metered, err := readaloud.NewMeteredSynthesizer(provider, costs, readaloud.StandardCharacterNanos)
	if err != nil {
		return nil, nil, err
	}
	access := &readaloud.Access{Entitlements: entitlements, Development: development, Consents: consents, JournalCharacters: c.journal, PreviewCharacters: c.preview, Now: time.Now}
	var gatewayAccess gateway.ReadAloudAccess = access
	authorize := access.Authorize
	if simulator != nil {
		testAccess := simulatortest.Access{Normal: access, Simulator: simulator}
		gatewayAccess, authorize = testAccess, testAccess.Authorize
	}
	s, err := readaloud.NewSessionService(ctx, readaloud.QuotaStore{DB: storage.voiceCatalog.DB}, metered, storage.voiceCatalog, authorize, time.Now)
	return s, gatewayAccess, err
}

type readAloudPrivacy struct {
	sessions *readaloud.SessionService
	next     privacy.CacheCleaner
}

func (c *readAloudPrivacy) DeletePrincipal(ctx context.Context, p attestation.Principal) error {
	if c.sessions != nil {
		c.sessions.CancelAccessKey(p.KeyID)
	}
	if c.next != nil {
		return c.next.DeletePrincipal(ctx, p)
	}
	return nil
}
