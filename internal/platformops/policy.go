// Package platformops owns versioned operational policy and aggregate telemetry.
package platformops

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/quota"
)

var (
	ErrInvalid  = errors.New("invalid operations policy")
	ErrConflict = errors.New("operations policy version conflict")
	ErrNotFound = errors.New("operations revision not found")
	ErrPaused   = errors.New("cloud function is paused")
)

type AppPolicy struct {
	MonthlyBudgetNanos int64    `json:"monthlyBudgetNanos"`
	DailyTokens        int      `json:"dailyTokens"`
	MonthlyTokens      int      `json:"monthlyTokens"`
	FreeDailyTokens    int      `json:"freeDailyTokens"`
	FreeMonthlyTokens  int      `json:"freeMonthlyTokens"`
	FreeDailySessions  int      `json:"freeDailySessions"`
	VoicePeriodMinutes int      `json:"voicePeriodMinutes"`
	Paused             bool     `json:"paused"`
	PausedOperations   []string `json:"pausedOperations"`
}

type Policy struct {
	Automation              *AutomationPolicy    `json:"automation,omitempty"`
	MonthlyBudgetNanos      int64                `json:"monthlyBudgetNanos"`
	MaxConcurrent           int                  `json:"maxConcurrent"`
	BudgetWarningPercent    int                  `json:"budgetWarningPercent"`
	ErrorWarningPercent     int                  `json:"errorWarningPercent"`
	SlowWarningMilliseconds int                  `json:"slowWarningMilliseconds"`
	MinimumSamples          int                  `json:"minimumSamples"`
	Apps                    map[string]AppPolicy `json:"apps"`
}

type Revision struct {
	ID          string     `json:"id"`
	BaseVersion string     `json:"baseVersion"`
	Policy      Policy     `json:"policy"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
}

type Reader interface {
	Current(context.Context) (Revision, error)
}

func Operations(app string) []string {
	if app == "journal" {
		return []string{"journal.organize", "journal.voice"}
	}
	if app != "health" {
		return nil
	}
	out := make([]string, 0, len(contracts.OperationValues()))
	for _, op := range contracts.OperationValues() {
		out = append(out, string(op))
	}
	return out
}

func Operation(app, op string) string {
	if app == "journal" {
		if strings.HasPrefix(op, "journal.organize.") {
			return "journal.organize"
		}
		if strings.HasPrefix(op, "journal.voice.") {
			return "journal.voice"
		}
	}
	return op
}

func (p Policy) Validate() error {
	if err := p.AutomationRules().Validate(); err != nil {
		return err
	}
	const maxBudget = 1_000_000 * costcontrol.NanosPerCNY
	if p.MonthlyBudgetNanos <= 0 || p.MonthlyBudgetNanos > maxBudget || p.MaxConcurrent < 1 || p.MaxConcurrent > 100 ||
		p.BudgetWarningPercent < 1 || p.BudgetWarningPercent > 100 || p.ErrorWarningPercent < 1 || p.ErrorWarningPercent > 100 ||
		p.SlowWarningMilliseconds < 100 || p.SlowWarningMilliseconds > 840_000 || p.MinimumSamples < 10 || p.MinimumSamples > 10000 || len(p.Apps) != 2 {
		return ErrInvalid
	}
	for _, app := range []string{"health", "journal"} {
		a, ok := p.Apps[app]
		if !ok || a.MonthlyBudgetNanos < 0 || a.MonthlyBudgetNanos > maxBudget || a.DailyTokens < 1 || a.MonthlyTokens < a.DailyTokens || a.MonthlyTokens > 1_000_000_000_000 {
			return ErrInvalid
		}
		if app == "health" {
			minimum := max(6, 2*a.FreeDailySessions) * contracts.MaxFreeRecognitionSessionReservationTokens
			if a.FreeDailySessions < 0 || a.FreeDailySessions > 100 || a.FreeDailyTokens < minimum || a.FreeMonthlyTokens < a.FreeDailyTokens || a.FreeMonthlyTokens > 1_000_000_000_000 || a.VoicePeriodMinutes != 0 {
				return ErrInvalid
			}
		} else if a.VoicePeriodMinutes < 1 || a.VoicePeriodMinutes > 10000 || a.FreeDailyTokens != 0 || a.FreeMonthlyTokens != 0 || a.FreeDailySessions != 0 {
			return ErrInvalid
		}
		allowed := map[string]bool{}
		for _, op := range Operations(app) {
			allowed[op] = true
		}
		for _, op := range a.PausedOperations {
			if !allowed[op] {
				return ErrInvalid
			}
			delete(allowed, op)
		}
	}
	return nil
}

func (p Policy) Check(app, operation string) error {
	a, ok := p.Apps[app]
	if !ok {
		return ErrInvalid
	}
	op := Operation(app, operation)
	valid := false
	for _, candidate := range Operations(app) {
		valid = valid || candidate == op
	}
	if !valid {
		return ErrInvalid
	}
	if a.Paused {
		return ErrPaused
	}
	for _, paused := range a.PausedOperations {
		if op == paused {
			return ErrPaused
		}
	}
	return nil
}

func Check(ctx context.Context, reader Reader, app, operation string) error {
	if reader == nil {
		return nil
	}
	r, err := reader.Current(ctx)
	if err != nil {
		return err
	}
	if err = r.Policy.Check(app, operation); err != nil {
		return err
	}
	if automated, ok := reader.(interface {
		CheckAutomation(context.Context, string, string) error
	}); ok {
		return automated.CheckAutomation(ctx, app, operation)
	}
	return nil
}

func QuotaResolver(reader Reader, app string, base quota.Limits, free bool) func(context.Context) (quota.Limits, error) {
	return func(ctx context.Context) (quota.Limits, error) {
		r, err := reader.Current(ctx)
		if err != nil {
			return quota.Limits{}, err
		}
		a, ok := r.Policy.Apps[app]
		if !ok {
			return quota.Limits{}, ErrInvalid
		}
		out := base
		out.DailyTokensPerTransaction, out.MonthlyTokensPerTransaction = a.DailyTokens, a.MonthlyTokens
		if free {
			out.DailyTokensPerTransaction, out.MonthlyTokensPerTransaction = a.FreeDailyTokens, a.FreeMonthlyTokens
		}
		return out, nil
	}
}
