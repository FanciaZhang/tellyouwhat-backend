package config

import (
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func TestHealthPromptDefaultsDoNotRequireDisabledJournalConfiguration(t *testing.T) {
	t.Setenv("JOURNAL_ARK_TIMEOUT_SECONDS", "invalid-disabled-setting")
	t.Setenv("JOURNAL_ARK_LITE_MODEL_ID", "")
	t.Setenv("JOURNAL_ARK_PRO_MODEL_ID", "")
	defaults, err := LoadPromptDefaults(AppConfig{Registry: appregistry.App{ID: appregistry.Health}}, AppConfig{Registry: appregistry.App{ID: appregistry.Birthday}})
	if err != nil || len(defaults) != len(contracts.OperationValues()) {
		t.Fatal("Health startup depends on disabled Journal", err, len(defaults))
	}
	for scope, policy := range defaults {
		if !strings.HasPrefix(scope, "health:") || policy.Validate(scope) != nil {
			t.Fatal("unexpected selected prompt", scope)
		}
	}
	if _, err := LoadPromptDefaults(); err == nil {
		t.Fatal("admin must still validate all configured scopes")
	}
	t.Setenv("JOURNAL_ARK_TIMEOUT_SECONDS", "90")
	t.Setenv("JOURNAL_ARK_LITE_MODEL_ID", "ep-lite")
	t.Setenv("JOURNAL_ARK_PRO_MODEL_ID", "ep-pro")
	all, err := LoadPromptDefaults()
	if err != nil || len(all) != len(promptconfig.Scopes()) {
		t.Fatal("existing combined production defaults changed", err)
	}
}
