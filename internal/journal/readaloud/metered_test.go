package readaloud

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

type spendStore struct {
	attempts                  []costcontrol.Attempt
	actual                    int64
	known                     bool
	finishContextError        error
	reserveError, settleError error
}

func (s *spendStore) Reserve(_ context.Context, a costcontrol.Attempt, _ costcontrol.Limits) error {
	if s.reserveError != nil {
		return s.reserveError
	}
	s.attempts = append(s.attempts, a)
	return nil
}
func (s *spendStore) Settle(ctx context.Context, _ string, actual int64, known bool, _ time.Time) error {
	s.actual, s.known, s.finishContextError = actual, known, ctx.Err()
	return s.settleError
}

type spendingProvider struct {
	calls  int
	usage  SynthesisUsage
	err    error
	cancel context.CancelFunc
}

func (p *spendingProvider) Synthesize(_ context.Context, _ Synthesis, emit func([]byte) error) (SynthesisUsage, error) {
	p.calls++
	if p.cancel != nil {
		p.cancel()
	}
	if p.err != nil {
		return p.usage, p.err
	}
	return p.usage, emit([]byte{1, 2})
}
func metered(t *testing.T, store *spendStore, provider Synthesizer) *MeteredSynthesizer {
	t.Helper()
	c, err := costcontrol.New(store, costcontrol.Limits{MonthlyBudgetNanos: costcontrol.NanosPerCNY * 10, MaxConcurrent: 2, LeaseDuration: time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewMeteredSynthesizer(provider, c, StandardCharacterNanos)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestMeteredSpeechCountsUnicodeAndSettlesProviderUsage(t *testing.T) {
	store := &spendStore{}
	p := &spendingProvider{usage: SynthesisUsage{Characters: 7, UsageKnown: true, AudioBytes: 2}}
	s := metered(t, store, p)
	input := validSynthesis()
	input.Text = "你a e\u0301🙂\n"
	_, err := s.Synthesize(context.Background(), input, func([]byte) error { return nil })
	if err != nil || len(store.attempts) != 1 || store.attempts[0].ReservedNanos != 7*StandardCharacterNanos || store.attempts[0].Operation != "journal.read_aloud" || !store.known || store.actual != 7*StandardCharacterNanos {
		t.Fatal("wrong character reservation or actual settlement", err, store)
	}
	if _, err = CharacterCount(string([]byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 counted")
	}
}
func TestMeteredSpeechDeniesSpendingBeforeCallingProvider(t *testing.T) {
	for _, denied := range []error{costcontrol.ErrBudgetExceeded, costcontrol.ErrConcurrencyExceeded, costcontrol.ErrProtectionActive, costcontrol.ErrPaused} {
		store := &spendStore{reserveError: denied}
		p := &spendingProvider{}
		_, err := metered(t, store, p).Synthesize(context.Background(), validSynthesis(), func([]byte) error { return nil })
		if !errors.Is(err, denied) || p.calls != 0 {
			t.Fatal("paid call passed rejected reservation", err)
		}
	}
}
func TestMeteredSpeechKeepsUnknownReservationAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &spendStore{}
	p := &spendingProvider{cancel: cancel, err: context.Canceled}
	_, err := metered(t, store, p).Synthesize(ctx, validSynthesis(), func([]byte) error { return nil })
	if !errors.Is(err, context.Canceled) || store.known || len(store.attempts) != 1 || store.finishContextError != nil {
		t.Fatal("cancelled request released unknown cost or prevented settlement", err, store)
	}
}
func TestMeteredSpeechReportsUnsettledCostWithoutLeakingStorageErrors(t *testing.T) {
	store := &spendStore{settleError: errors.New("private storage details")}
	p := &spendingProvider{}
	_, err := metered(t, store, p).Synthesize(context.Background(), validSynthesis(), func([]byte) error { return nil })
	if !errors.Is(err, ErrSettlement) || err.Error() != ErrSettlement.Error() {
		t.Fatal("unsafe settlement error", err)
	}
}

func TestMeteredSpeechDoesNotReleaseZeroUsageForSuccessfulAudio(t *testing.T) {
	store := &spendStore{}
	p := &spendingProvider{usage: SynthesisUsage{UsageKnown: true}}
	_, err := metered(t, store, p).Synthesize(context.Background(), validSynthesis(), func([]byte) error { return nil })
	if err != nil || store.known || len(store.attempts) != 1 {
		t.Fatal("nonempty synthesis falsely settled as free", err, store)
	}
}
