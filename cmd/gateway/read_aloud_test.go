package main

import (
	"context"
	"testing"
)

func TestReadAloudConfigurationRequiresExplicitEnableAndAllowance(t *testing.T) {
	for _, key := range []string{"JOURNAL_TTS_ENABLED", "JOURNAL_TTS_API_KEY_FILE", "JOURNAL_TTS_MONTHLY_CHARACTERS", "JOURNAL_TTS_PREVIEW_DAILY_CHARACTERS"} {
		t.Setenv(key, "")
	}
	if cfg, err := loadReadAloudConfig(); err != nil || cfg != nil {
		t.Fatal("unconfigured TTS enabled", cfg, err)
	}
	t.Setenv("JOURNAL_TTS_ENABLED", "true")
	if _, err := loadReadAloudConfig(); err == nil {
		t.Fatal("unbounded TTS enabled")
	}
	t.Setenv("JOURNAL_TTS_API_KEY_FILE", "/synthetic/private-key")
	t.Setenv("JOURNAL_TTS_MONTHLY_CHARACTERS", "10000")
	cfg, err := loadReadAloudConfig()
	if err != nil || cfg.journal != 10000 || cfg.preview != 1000 {
		t.Fatal(cfg, err)
	}
	if _, _, err := configuredReadAloud(context.Background(), cfg, appStorage{}, nil, nil, nil, nil, false); err == nil {
		t.Fatal("TTS started without durable storage and cost protection")
	}
	t.Setenv("JOURNAL_TTS_PREVIEW_DAILY_CHARACTERS", "-1")
	if _, err := loadReadAloudConfig(); err == nil {
		t.Fatal("invalid preview allowance accepted")
	}
	t.Setenv("JOURNAL_TTS_ENABLED", "yes")
	if _, err := loadReadAloudConfig(); err == nil {
		t.Fatal("ambiguous enable flag accepted")
	}
}
