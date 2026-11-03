package readaloud

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

type reservationFixture struct {
	replay                 bool
	beginError             error
	runningError           error
	state                  string
	actual                 int
	known                  bool
	settlementContextError error
}

func (f *reservationFixture) Begin(_ context.Context, g Grant, input UnitRequest, _ time.Time) (RequestRecord, bool, error) {
	return RequestRecord{UnitRequest: input, Subject: g.Subject}, f.replay, f.beginError
}
func (f *reservationFixture) MarkRunning(context.Context, Subject, string) error {
	return f.runningError
}
func (f *reservationFixture) Finish(ctx context.Context, _ Subject, _ string, state string, actual int, known bool) error {
	f.state, f.actual, f.known, f.settlementContextError = state, actual, known, ctx.Err()
	return nil
}
func TestAccountSpeechNeverRepeatsClaimedOrDeniedSynthesis(t *testing.T) {
	for _, f := range []*reservationFixture{{replay: true}, {beginError: ErrQuotaExceeded}} {
		provider := &spendingProvider{}
		s, _ := NewAccountSynthesizer(f, provider, quotaGrant(time.Now()), "revision", time.Now)
		if _, err := s.Synthesize(context.Background(), validSynthesis(), func([]byte) error { return nil }); err == nil || provider.calls != 0 {
			t.Fatal("duplicate or denied request reached provider", err)
		}
	}
}
func TestAccountSpeechSeparatesAudioOutcomeFromUnknownUsage(t *testing.T) {
	f := &reservationFixture{}
	s, _ := NewAccountSynthesizer(f, &spendingProvider{}, quotaGrant(time.Now()), "revision", time.Now)
	if _, err := s.Synthesize(context.Background(), validSynthesis(), func([]byte) error { return nil }); err != nil || f.state != "succeeded" || f.known || f.actual != 0 {
		t.Fatal("successful audio lost or unknown spending released", f, err)
	}
}

func TestAccountSpeechReleasesReservationWhenStartFails(t *testing.T) {
	f := &reservationFixture{runningError: context.DeadlineExceeded}
	p := &spendingProvider{}
	s, _ := NewAccountSynthesizer(f, p, quotaGrant(time.Now()), "revision", time.Now)
	_, err := s.Synthesize(context.Background(), validSynthesis(), func([]byte) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || p.calls != 0 || !f.known || f.actual != 0 || f.state != "failed" || f.settlementContextError != nil {
		t.Fatal("unsubmitted request retained quota", f, err)
	}
}

func TestAccountSpeechDoesNotTreatSuccessfulAudioAsFree(t *testing.T) {
	f := &reservationFixture{}
	s, _ := NewAccountSynthesizer(f, &spendingProvider{usage: SynthesisUsage{UsageKnown: true}}, quotaGrant(time.Now()), "revision", time.Now)
	_, err := s.Synthesize(context.Background(), validSynthesis(), func([]byte) error { return nil })
	if err != nil || f.known || f.state != "succeeded" {
		t.Fatal("nonempty synthesis falsely settled as free", f, err)
	}
}
func TestAccountSpeechReleasesKnownPreProviderRejectionAndRetainsCancelledUsage(t *testing.T) {
	for _, failure := range []error{costcontrol.ErrBudgetExceeded, costcontrol.ErrConcurrencyExceeded, costcontrol.ErrProtectionActive, costcontrol.ErrPaused} {
		f := &reservationFixture{}
		s, _ := NewAccountSynthesizer(f, &spendingProvider{err: failure}, quotaGrant(time.Now()), "revision", time.Now)
		_, err := s.Synthesize(context.Background(), validSynthesis(), func([]byte) error { return nil })
		if !errors.Is(err, failure) || !f.known || f.actual != 0 {
			t.Fatal("unsubmitted request consumed characters", f, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &reservationFixture{}
	s, _ := NewAccountSynthesizer(f, &spendingProvider{err: context.Canceled, cancel: cancel}, quotaGrant(time.Now()), "revision", time.Now)
	_, err := s.Synthesize(ctx, validSynthesis(), func([]byte) error { return nil })
	if !errors.Is(err, context.Canceled) || f.known || f.state != "cancelled" || f.settlementContextError != nil {
		t.Fatal("cancelled request lost accounting", f, err)
	}
}
