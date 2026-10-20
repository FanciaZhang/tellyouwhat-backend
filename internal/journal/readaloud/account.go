package readaloud

import (
	"context"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

type ReservationRepository interface {
	Begin(context.Context, Grant, UnitRequest, time.Time) (RequestRecord, bool, error)
	MarkRunning(context.Context, Subject, string) error
	Finish(context.Context, Subject, string, string, int, bool) error
}

// Each instance belongs to one server-verified allowance and voice revision.
// A repeated request never starts another provider call, even after a restart.
type AccountSynthesizer struct {
	store    ReservationRepository
	provider Synthesizer
	grant    Grant
	revision string
	now      func() time.Time
}

func NewAccountSynthesizer(store ReservationRepository, provider Synthesizer, grant Grant, revision string, now func() time.Time) (*AccountSynthesizer, error) {
	if now == nil {
		now = time.Now
	}
	if store == nil || provider == nil || revision == "" || !grant.Valid(now()) {
		return nil, ErrInput
	}
	return &AccountSynthesizer{store, provider, grant, revision, now}, nil
}

func (s *AccountSynthesizer) Synthesize(ctx context.Context, input Synthesis, emit func([]byte) error) (SynthesisUsage, error) {
	if s == nil || emit == nil {
		return SynthesisUsage{}, ErrInput
	}
	request, err := NewUnitRequest(input, s.revision, s.grant.Purpose)
	if err != nil {
		return SynthesisUsage{}, err
	}
	if err := ctx.Err(); err != nil {
		return SynthesisUsage{}, err
	}
	r, replay, err := s.store.Begin(ctx, s.grant, request, s.now())
	if err != nil {
		return SynthesisUsage{}, err
	}
	if replay {
		return SynthesisUsage{}, ErrRequestConflict
	}
	return s.synthesizeClaimed(ctx, input, r, emit)
}

func (s *AccountSynthesizer) synthesizeClaimed(ctx context.Context, input Synthesis, r RequestRecord, emit func([]byte) error) (SynthesisUsage, error) {
	var err error
	if err = s.store.MarkRunning(ctx, s.grant.Subject, r.ID); err != nil {
		settleContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if e := s.store.Finish(settleContext, s.grant.Subject, r.ID, "failed", 0, true); e != nil {
			return SynthesisUsage{}, errors.Join(err, ErrSettlement)
		}
		return SynthesisUsage{}, err
	}
	usage, err := s.provider.Synthesize(ctx, input, emit)
	known := usage.UsageKnown && usage.Characters >= 0 && usage.Characters <= MaximumTextBytes*2 && (usage.Characters > 0 || err != nil)
	state, actual := "unknown", 0
	if known {
		actual = usage.Characters
	}
	if err == nil {
		state = "succeeded"
	} else if known {
		state = "failed"
	}
	if costcontrol.IsCancellation(ctx, err) {
		state = "cancelled"
	}
	// The project controller rejected admission before a paid call was made.
	if errors.Is(err, costcontrol.ErrBudgetExceeded) || errors.Is(err, costcontrol.ErrConcurrencyExceeded) || errors.Is(err, costcontrol.ErrProtectionActive) {
		state, actual, known = "failed", 0, true
	}
	settleContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e := s.store.Finish(settleContext, s.grant.Subject, r.ID, state, actual, known); e != nil {
		return usage, errors.Join(err, ErrSettlement)
	}
	return usage, err
}
