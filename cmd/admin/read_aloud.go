package main

import (
	"database/sql"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/tellyouwhat/backend/internal/adminportal"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func readAloudServices(database *sql.DB, limits costcontrol.Limits) (adminportal.VoiceDiscovery, *readaloud.CatalogProbe, error) {
	var discovery adminportal.VoiceDiscovery
	if path := os.Getenv("JOURNAL_TTS_INVENTORY_CREDENTIALS_FILE"); path != "" {
		value, err := readaloud.NewDiscoveryFromFile(path)
		if err != nil {
			return nil, nil, err
		}
		discovery = value
	}
	path := os.Getenv("JOURNAL_TTS_API_KEY_FILE")
	if path == "" {
		return discovery, nil, nil
	}
	provider, err := readaloud.NewProviderFromFile(path)
	if err != nil {
		return nil, nil, err
	}
	daily := 33_333
	if raw := os.Getenv("JOURNAL_TTS_PROBE_DAILY_CHARACTERS"); raw != "" {
		daily, err = strconv.Atoi(raw)
		if err != nil || daily < 1 || daily > 100_000_000 {
			return nil, nil, errors.New("invalid read-aloud probe allowance")
		}
	}
	costs, err := costcontrol.New(mysqlstore.NewCostControlStore(database), limits, time.Now)
	if err != nil {
		return nil, nil, err
	}
	providerWithCosts, err := readaloud.NewMeteredSynthesizer(provider, costs, readaloud.StandardCharacterNanos)
	if err != nil {
		return nil, nil, err
	}
	return discovery, &readaloud.CatalogProbe{Store: readaloud.CatalogStore{DB: database}, Quotas: &readaloud.QuotaStore{DB: database}, Provider: providerWithCosts, DailyCharacters: daily}, nil
}
