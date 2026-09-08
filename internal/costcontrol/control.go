// Package costcontrol enforces one provider-spend ceiling across all apps and
// processes without storing prompts, responses, media, or user identifiers.
package costcontrol

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
)

const NanosPerCNY int64 = 1_000_000_000

var (
	ErrBudgetExceeded        = errors.New("project AI monthly budget exceeded")
	ErrConcurrencyExceeded   = errors.New("project AI concurrency exceeded")
	ErrConfigurationConflict = errors.New("project AI budget configuration conflicts with the active month")
	ErrInvalidAttempt        = errors.New("invalid AI cost attempt")
)

type Limits struct {
	MonthlyBudgetNanos int64
	MaxConcurrent      int
	LeaseDuration      time.Duration
}

func (limits Limits) Valid() bool {
	return limits.MonthlyBudgetNanos > 0 && limits.MaxConcurrent > 0 &&
		limits.LeaseDuration >= time.Minute && limits.LeaseDuration <= time.Hour
}

type Attempt struct {
	ID, AppID, Operation, Meter string
	MonthStart                  time.Time
	ReservedNanos               int64
	CreatedAt, LeaseExpiresAt   time.Time
}

func (attempt Attempt) Valid() bool {
	_, err := uuid.Parse(attempt.ID)
	return err == nil && attempt.AppID != "" && attempt.Operation != "" && attempt.Meter != "" &&
		attempt.ReservedNanos > 0 && !attempt.MonthStart.IsZero() &&
		!attempt.CreatedAt.IsZero() && attempt.LeaseExpiresAt.After(attempt.CreatedAt)
}

type Store interface {
	Reserve(context.Context, Attempt, Limits) error
	Settle(context.Context, string, int64, bool, time.Time) error
}

type Outcome struct {
	UsageKnown                bool
	Success                   bool
	InputTokens, OutputTokens int
	Model                     string
}
type OutcomeRecorder interface {
	RecordOutcome(context.Context, string, Outcome, time.Time) error
}
type RejectionRecorder interface {
	RecordRejection(context.Context, string, string, string, time.Time) error
}

type Controller struct {
	store  Store
	limits Limits
	now    func() time.Time
}

func New(store Store, limits Limits, now func() time.Time) (*Controller, error) {
	if store == nil || !limits.Valid() {
		return nil, ErrInvalidAttempt
	}
	if now == nil {
		now = time.Now
	}
	return &Controller{store: store, limits: limits, now: now}, nil
}

func (controller *Controller) Reserve(ctx context.Context, appID, operation, meter string, reservedNanos int64) (*Lease, error) {
	if controller == nil || controller.store == nil || reservedNanos <= 0 {
		return nil, ErrInvalidAttempt
	}
	now := controller.now().UTC()
	attempt := Attempt{
		ID: uuid.NewString(), AppID: appID, Operation: operation, Meter: meter,
		MonthStart:    time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC),
		ReservedNanos: reservedNanos, CreatedAt: now, LeaseExpiresAt: now.Add(controller.limits.LeaseDuration),
	}
	if !attempt.Valid() {
		return nil, ErrInvalidAttempt
	}
	if err := controller.store.Reserve(ctx, attempt, controller.limits); err != nil {
		if recorder, ok := controller.store.(RejectionRecorder); ok {
			reason := ""
			if errors.Is(err, ErrBudgetExceeded) {
				reason = "budget"
			} else if errors.Is(err, ErrConcurrencyExceeded) {
				reason = "concurrency"
			}
			if reason != "" {
				_ = recorder.RecordRejection(ctx, appID, operation, reason, now)
			}
		}
		return nil, err
	}
	return &Lease{store: controller.store, attemptID: attempt.ID, now: controller.now}, nil
}

func (lease *Lease) Finish(ctx context.Context, actual int64, known bool, outcome Outcome) error {
	err := lease.Settle(ctx, actual, known)
	if lease != nil {
		if recorder, ok := lease.store.(OutcomeRecorder); ok {
			err = errors.Join(err, recorder.RecordOutcome(ctx, lease.attemptID, outcome, lease.now().UTC()))
		}
	}
	return err
}

type Lease struct {
	store     Store
	attemptID string
	now       func() time.Time
}

func (lease *Lease) Settle(ctx context.Context, actualNanos int64, known bool) error {
	if lease == nil || lease.store == nil || actualNanos < 0 || lease.attemptID == "" {
		return ErrInvalidAttempt
	}
	return lease.store.Settle(ctx, lease.attemptID, actualNanos, known, lease.now().UTC())
}

type TokenPrice struct {
	InputNanosPerMillionTokens  int64
	OutputNanosPerMillionTokens int64
}

func (price TokenPrice) Valid() bool {
	return price.InputNanosPerMillionTokens > 0 && price.OutputNanosPerMillionTokens > 0
}

func (price TokenPrice) Cost(inputTokens, outputTokens int) (int64, error) {
	if !price.Valid() || inputTokens < 0 || outputTokens < 0 {
		return 0, ErrInvalidAttempt
	}
	input, ok := scaledCeiling(int64(inputTokens), price.InputNanosPerMillionTokens, 1_000_000)
	if !ok {
		return 0, ErrInvalidAttempt
	}
	output, ok := scaledCeiling(int64(outputTokens), price.OutputNanosPerMillionTokens, 1_000_000)
	if !ok || input > math.MaxInt64-output {
		return 0, ErrInvalidAttempt
	}
	return input + output, nil
}

type DurationPrice struct{ NanosPerHour int64 }

func (price DurationPrice) Cost(milliseconds int) (int64, error) {
	if price.NanosPerHour <= 0 || milliseconds < 0 {
		return 0, ErrInvalidAttempt
	}
	result, ok := scaledCeiling(int64(milliseconds), price.NanosPerHour, 60*60*1000)
	if !ok {
		return 0, ErrInvalidAttempt
	}
	return result, nil
}

func scaledCeiling(count, unitPrice, denominator int64) (int64, bool) {
	if count == 0 {
		return 0, true
	}
	if count < 0 || unitPrice <= 0 || denominator <= 0 || count > math.MaxInt64/unitPrice {
		return 0, false
	}
	product := count * unitPrice
	if product > math.MaxInt64-(denominator-1) {
		return 0, false
	}
	return (product + denominator - 1) / denominator, true
}
