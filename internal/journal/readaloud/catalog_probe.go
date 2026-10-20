package readaloud

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
)

type CatalogProbe struct {
	Store           CatalogStore
	Quotas          ReservationRepository
	Provider        Synthesizer
	DailyCharacters int
	Now             func() time.Time
}
type ProbeResult struct {
	Voice      Voice  `json:"voice"`
	Audio      []byte `json:"audio,omitempty"`
	SampleRate int    `json:"sampleRate"`
	Replayed   bool   `json:"replayed"`
}
type synthesisFunc func(context.Context, Synthesis, func([]byte) error) (SynthesisUsage, error)

func (f synthesisFunc) Synthesize(ctx context.Context, input Synthesis, emit func([]byte) error) (SynthesisUsage, error) {
	return f(ctx, input, emit)
}

func (s CatalogProbe) Verify(ctx context.Context, input VoiceConfiguration, m aiconfig.Mutation) (ProbeResult, error) {
	if s.Provider == nil || s.Quotas == nil || s.DailyCharacters <= 0 || s.DailyCharacters > 100_000_000 {
		return ProbeResult{}, ErrInput
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	voice := input.Voice()
	previous, err := s.Store.ClaimProbe(ctx, m, voice, now())
	if err != nil {
		return ProbeResult{}, err
	}
	if previous != nil {
		return ProbeResult{Voice: *previous, SampleRate: SampleRate, Replayed: true}, nil
	}
	clock := now().UTC()
	day := time.Date(clock.Year(), clock.Month(), clock.Day(), 0, 0, 0, 0, time.UTC)
	grant := Grant{Subject: Subject{"journal", "catalog-voice-probes"}, Purpose: "preview", Start: day, End: day.Add(24 * time.Hour), Limit: s.DailyCharacters}
	account, err := NewAccountSynthesizer(s.Quotas, s.Provider, grant, voice.policyHash(), now)
	if err != nil {
		settleContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if e := s.Store.FinishProbe(settleContext, m, voice, false, now()); e != nil {
			return ProbeResult{}, errors.Join(err, ErrSettlement)
		}
		return ProbeResult{}, err
	}
	parent := uuid.MustParse(m.RequestID)
	index := 0
	provider := synthesisFunc(func(ctx context.Context, input Synthesis, emit func([]byte) error) (SynthesisUsage, error) {
		// Stable child identities protect each case if the HTTP response is lost.
		input.RequestID = uuid.NewSHA1(parent, []byte(strconv.Itoa(index))).String()
		index++
		return account.Synthesize(ctx, input, emit)
	})
	probeContext, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	verified, audio, err := VerifyVoice(probeContext, voice, provider, now())
	settleContext, settleCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer settleCancel()
	if settleErr := s.Store.FinishProbe(settleContext, m, verified, err == nil, now()); settleErr != nil {
		return ProbeResult{}, errors.Join(err, ErrSettlement)
	}
	if err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{Voice: verified, Audio: audio, SampleRate: SampleRate}, nil
}
