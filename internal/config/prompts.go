package config

import "github.com/tellyouwhat/backend/internal/promptconfig"

// LoadPromptDefaults reads non-secret initialization values only.
func LoadPromptDefaults() (map[string]promptconfig.Policy, error) {
	timeout, err := prefixedInt("JOURNAL", "ARK_TIMEOUT_SECONDS", 90)
	if err != nil {
		return nil, err
	}
	defaults := promptconfig.Defaults(prefixedValue("JOURNAL", "ARK_LITE_MODEL_ID", ""), prefixedValue("JOURNAL", "ARK_PRO_MODEL_ID", ""), prefixedValue("JOURNAL", "VOICE_MODEL_ID", prefixedValue("JOURNAL", "ARK_PRO_MODEL_ID", "")), timeout)
	for scope, p := range defaults {
		if err := p.Validate(scope); err != nil {
			return nil, err
		}
	}
	return defaults, nil
}

func LoadCostDefaults() (AICostConfig, error) { return loadAICostConfig() }
