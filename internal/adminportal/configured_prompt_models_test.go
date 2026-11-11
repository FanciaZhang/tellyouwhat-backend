package adminportal

import (
	"context"
	"errors"
	"testing"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func TestConfiguredPromptModelsDoNotRequireCloudManagementOrTrustSubmittedPrices(t *testing.T) {
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 9600000000, OutputNanosPerMillionTokens: 48000000000}
	s := Server{config: Config{Environment: "development", WritesEnabled: true, ConfiguredPromptModels: map[string]costcontrol.TokenPrice{"lite": price, "pro": price, "voice": price}}}
	p := promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]
	p.Journal.Voice.Parameters.Price = &costcontrol.TokenPrice{InputNanosPerMillionTokens: 1, OutputNanosPerMillionTokens: 1}
	p.Journal.Voice.Parameters.FoundationModel = "submitted metadata"
	if err := s.resolvePromptModels(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if *p.Journal.Voice.Parameters.Price != price || p.Journal.Voice.Parameters.FoundationModel != "" {
		t.Fatal("submitted pricing or model metadata trusted")
	}
	if !s.promptWritesEnabled("journal") || s.promptWritesEnabled("health:meal_text_capture") {
		t.Fatal("configured Journal editing expanded into Health")
	}
	p.Journal.Voice.Parameters.Model = "unconfigured-model"
	if err := s.resolvePromptModels(context.Background(), &p); !errors.Is(err, promptconfig.ErrInvalid) {
		t.Fatal("unconfigured provider model admitted", err)
	}
	s.config.WritesEnabled = false
	if s.promptWritesEnabled("journal") {
		t.Fatal("global write switch bypassed")
	}
	s.config.WritesEnabled = true
	s.config.Environment = "production"
	if s.promptWritesEnabled("journal") {
		t.Fatal("development fallback enabled production writes")
	}
	s.config.AI = &AIConfig{WritesEnabled: false}
	if s.promptWritesEnabled("journal") {
		t.Fatal("cloud management write switch bypassed")
	}
}
