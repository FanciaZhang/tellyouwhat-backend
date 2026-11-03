package config

import (
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/platformops"
	"github.com/tellyouwhat/backend/internal/recognitionquota"
)

// LoadOperationsDefaults reads only non-secret deployment defaults. Published
// database policy takes precedence after initialization.
func LoadOperationsDefaults() (platformops.Policy, error) {
	cost, err := loadAICostConfig()
	if err != nil {
		return platformops.Policy{}, err
	}
	p := platformops.Policy{MonthlyBudgetNanos: cost.Limits.MonthlyBudgetNanos, MaxConcurrent: cost.Limits.MaxConcurrent, BudgetWarningPercent: 80, ErrorWarningPercent: 20, SlowWarningMilliseconds: 60000, MinimumSamples: 20, Apps: map[string]platformops.AppPolicy{}}
	for _, entry := range []struct{ id, prefix string }{{"health", "HEALTH"}, {"journal", "JOURNAL"}} {
		limits, err := loadPrefixedQuota(entry.prefix)
		if err != nil {
			return p, err
		}
		a := platformops.AppPolicy{DailyTokens: limits.DailyTokensPerTransaction, MonthlyTokens: limits.MonthlyTokensPerTransaction, PausedOperations: []string{}}
		if entry.id == "health" {
			free, err := loadFreeRecognitionQuota(entry.prefix, limits)
			if err != nil {
				return p, err
			}
			a.FreeDailyTokens = free.DailyTokensPerTransaction
			a.FreeMonthlyTokens = free.MonthlyTokensPerTransaction
			a.FreeDailySessions = recognitionquota.DailySessionLimit
		} else {
			a.VoicePeriodMinutes = voice.MonthlyMilliseconds / 60000
		}
		p.Apps[entry.id] = a
	}
	return p, p.Validate()
}
