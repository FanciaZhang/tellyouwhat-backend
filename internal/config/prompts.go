package config

import (
	"strings"

	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

// LoadPromptDefaults reads non-secret initialization values only.
func LoadPromptDefaults(apps ...AppConfig) (map[string]promptconfig.Policy, error) {
	selected := map[appregistry.AppID]bool{}
	for _, app := range apps {
		selected[app.Registry.ID] = true
	}
	timeout := 90
	if len(apps) == 0 || selected[appregistry.Journal] {
		var err error
		timeout, err = prefixedInt("JOURNAL", "ARK_TIMEOUT_SECONDS", 90)
		if err != nil {
			return nil, err
		}
	}
	defaults := promptconfig.Defaults(prefixedValue("JOURNAL", "ARK_LITE_MODEL_ID", ""), prefixedValue("JOURNAL", "ARK_PRO_MODEL_ID", ""), prefixedValue("JOURNAL", "VOICE_MODEL_ID", prefixedValue("JOURNAL", "ARK_PRO_MODEL_ID", "")), timeout)
	for scope, p := range defaults {
		if len(apps) > 0 && !selected[appregistry.AppID(strings.SplitN(scope, ":", 2)[0])] {
			delete(defaults, scope)
			continue
		}
		if err := p.Validate(scope); err != nil {
			return nil, err
		}
	}
	return defaults, nil
}

func LoadCostDefaults() (AICostConfig, error) { return loadAICostConfig() }
