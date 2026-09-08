// Package airollout controls native Ark changes for explicitly owned Health endpoints.
package airollout

import (
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"math/big"
)

// CatalogVersion identifies the application protocol compatibility profile.
// Ark's capability flags alone do not certify reasoning levels or media payloads.
const CatalogVersion = "health-responses-scoped-2026-09-08"

type Capability struct {
	Model   arkcontrol.FoundationModel `json:"model"`
	Efforts []string                   `json:"efforts"`
	Audio   bool                       `json:"audio"`
	Images  bool                       `json:"images"`
	Search  bool                       `json:"search"`
}

func Catalog() []Capability {
	return []Capability{
		{arkcontrol.FoundationModel{Name: "doubao-seed-2-0-mini", Version: "260215"}, []string{"minimal", "low", "medium", "high"}, false, true, true},
		{arkcontrol.FoundationModel{Name: "doubao-seed-2-0-mini", Version: "260428"}, []string{"minimal", "low", "medium", "high"}, true, true, true},
		{arkcontrol.FoundationModel{Name: "doubao-seed-2-0-lite", Version: "260428"}, []string{"minimal", "low", "medium", "high"}, true, true, true},
	}
}
func Supports(m arkcontrol.FoundationModel, op contracts.Operation, p contracts.ExecutionPolicy) bool {
	if p.Validate(op) != nil {
		return false
	}
	for _, c := range Catalog() {
		if c.Model != m {
			continue
		}
		if op == contracts.OperationVoiceTranscription && !c.Audio {
			return false
		}
		if (op == contracts.OperationMealPhotoCapture || op == contracts.OperationHydrationCupEstimate || op == contracts.OperationMealDecision) && !c.Images {
			return false
		}
		if p.WebSearchEnabled && !c.Search {
			return false
		}
		for _, effort := range c.Efforts {
			if effort == p.ReasoningEffort || p.ReasoningEffort == "" {
				return true
			}
		}
	}
	return false
}

// Price uses the highest undiscounted or account price across all context tiers.
// Input also includes audio, since usage may aggregate media and text tokens.
func Price(a arkcontrol.Activation) (costcontrol.TokenPrice, error) {
	var p costcontrol.TokenPrice
	if a.State != "Available" || a.Deprecated || a.Overdue {
		return p, ErrUnsupported
	}
	items := append([]arkcontrol.ChargeItem{}, a.Charges...)
	for _, tier := range a.Tiers {
		items = append(items, tier.Charges...)
	}

	for _, item := range items {
		if item.Type != "InferencePrompt" && item.Type != "InferenceCompletion" && item.Type != "AudioPrompt" {
			continue
		}
		var multiplier int64
		switch item.Unit {
		case "千tokens":
			multiplier = 1_000_000_000_000
		case "百万tokens":
			multiplier = 1_000_000_000
		default:
			return p, ErrUnsupported
		}
		for _, raw := range []string{item.Price.String(), item.OriginalPrice.String()} {
			if raw == "" {
				continue
			}
			n, ok := new(big.Rat).SetString(raw)
			if !ok || n.Sign() < 0 {
				return p, ErrUnsupported
			}
			n.Mul(n, big.NewRat(multiplier, 1)) // CNY / thousand -> nanos / million.
			rounded := new(big.Int).Quo(n.Num(), n.Denom())
			if new(big.Int).Mod(n.Num(), n.Denom()).Sign() != 0 {
				rounded.Add(rounded, big.NewInt(1))
			}
			if !rounded.IsInt64() {
				return p, ErrUnsupported
			}
			v := rounded.Int64()
			if item.Type == "InferenceCompletion" {
				p.OutputNanosPerMillionTokens = max(p.OutputNanosPerMillionTokens, v)
			} else {
				p.InputNanosPerMillionTokens = max(p.InputNanosPerMillionTokens, v)
			}

		}
	}
	if !p.Valid() {
		return p, ErrUnsupported
	}
	return p, nil
}
func ceiling(a, b costcontrol.TokenPrice) costcontrol.TokenPrice {
	return costcontrol.TokenPrice{InputNanosPerMillionTokens: max(a.InputNanosPerMillionTokens, b.InputNanosPerMillionTokens), OutputNanosPerMillionTokens: max(a.OutputNanosPerMillionTokens, b.OutputNanosPerMillionTokens)}
}
