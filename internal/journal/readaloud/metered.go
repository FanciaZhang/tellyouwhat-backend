package readaloud

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

// The configured price is CNY nanos per Unicode code point, not UTF-8 byte or
// user-perceived grapheme. Standard TTS 2.0 is 3 CNY per 10,000 characters.
const StandardCharacterNanos int64 = 300_000

var ErrSettlement = errors.New("read-aloud cost settlement unavailable")

type MeteredSynthesizer struct {
	provider          Synthesizer
	costs             *costcontrol.Controller
	nanosPerCharacter int64
}

func NewMeteredSynthesizer(provider Synthesizer, costs *costcontrol.Controller, nanosPerCharacter int64) (*MeteredSynthesizer, error) {
	if provider == nil || costs == nil || nanosPerCharacter <= 0 || nanosPerCharacter > costcontrol.NanosPerCNY {
		return nil, ErrInput
	}
	return &MeteredSynthesizer{provider: provider, costs: costs, nanosPerCharacter: nanosPerCharacter}, nil
}

func CharacterCount(text string) (int, error) {
	if !utf8.ValidString(text) {
		return 0, ErrInput
	}
	return utf8.RuneCountInString(text), nil
}

func (s *MeteredSynthesizer) Synthesize(ctx context.Context, input Synthesis, emit func([]byte) error) (SynthesisUsage, error) {
	if s == nil || !input.Valid() || emit == nil {
		return SynthesisUsage{}, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return SynthesisUsage{}, err
	}
	characters, _ := CharacterCount(input.Text)
	lease, err := s.costs.Reserve(ctx, "journal", "read_aloud", ResourceID, int64(characters)*s.nanosPerCharacter)
	if err != nil {
		return SynthesisUsage{}, err
	}
	usage, err := s.provider.Synthesize(ctx, input, emit)
	actual := int64(0)
	known := usage.UsageKnown && usage.Characters >= 0 && usage.Characters <= MaximumTextBytes*2 && (usage.Characters > 0 || err != nil)
	if known {
		actual = int64(usage.Characters) * s.nanosPerCharacter
	}
	// Never release a reservation merely because the client disconnected, the
	// provider response was truncated, or the result is unknown. Settle even when
	// the request context has been cancelled; the store retains unknown spending.
	settleContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	settleErr := lease.Finish(settleContext, actual, known, costcontrol.Outcome{
		Cancelled: costcontrol.IsCancellation(ctx, err), UsageKnown: known, Success: err == nil, Model: ResourceID,
	})
	if settleErr != nil {
		return usage, errors.Join(err, ErrSettlement)
	}
	return usage, err
}
