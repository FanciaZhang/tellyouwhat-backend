package quota

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
)

const MaximumJobAttempts = 3

var ErrAttemptBudgetUnavailable = errors.New("job attempt budget is unavailable")

// JobAttempt binds one worker claim to the gateway's authenticated reservation.
// Retries reserve tokens in their own admission window; the first attempt uses
// the original prepayment. Recognition session counts are independent.
type JobAttempt struct {
	TransactionID  string
	DeviceID       string
	ReservationID  string
	ReservedTokens int
	Number         int
}

func (attempt JobAttempt) Valid() bool {
	return attempt.TransactionID != "" && attempt.DeviceID != "" && attempt.ReservationID != "" &&
		attempt.ReservedTokens >= 0 && attempt.Number >= 1 && attempt.Number <= MaximumJobAttempts
}

func (attempt JobAttempt) TokenReservationID() string {
	if attempt.Number == 1 {
		return attempt.ReservationID
	}
	return contracts.BodySHA256([]byte("job-attempt\n" + attempt.ReservationID + "\n" + strconv.Itoa(attempt.Number)))
}

type JobAttemptBudget interface {
	TokenReconciler
	ReserveJobAttempt(context.Context, JobAttempt, time.Time) (string, error)
}

func (limiter *MemoryLimiter) ReserveJobAttempt(ctx context.Context, attempt JobAttempt, now time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if limiter == nil || !attempt.Valid() {
		return "", ErrInvalidReservation
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	root, exists := limiter.reservations[attempt.ReservationID]
	if !exists || !now.Before(root.expiresAt) || !root.Matches(attempt.TransactionID, attempt.ReservedTokens) ||
		root.DeviceID != attempt.DeviceID || (root.RootReservationID != "" && root.RootReservationID != attempt.ReservationID) ||
		(root.Attempt != 0 && root.Attempt != 1) {
		return "", ErrInvalidReservation
	}
	id := attempt.TokenReservationID()
	if attempt.Number == 1 {
		if root.Reconciled {
			return "", ErrInvalidReservation
		}
		root.RootReservationID, root.Attempt = attempt.ReservationID, 1
		limiter.reservations[id] = root
		return id, nil
	}
	if previous, exists := limiter.reservations[id]; exists && now.Before(previous.expiresAt) {
		if !previous.Matches(attempt.TransactionID, attempt.ReservedTokens) || previous.DeviceID != attempt.DeviceID ||
			previous.RootReservationID != attempt.ReservationID || previous.Attempt != attempt.Number || previous.Reconciled {
			return "", ErrInvalidReservation
		}
		return id, nil
	}
	day, month := now.UTC().Format("20060102"), now.UTC().Format("200601")
	dayKey, monthKey := "day:"+attempt.TransactionID, "month:"+attempt.TransactionID
	dayUsed, monthUsed := counterValue(limiter.tokens[dayKey], day), counterValue(limiter.tokens[monthKey], month)
	if limiter.limits.DailyTokensPerTransaction > 0 && attempt.ReservedTokens > limiter.limits.DailyTokensPerTransaction-dayUsed {
		return "", Exceeded(LimitDailyTokens)
	}
	if limiter.limits.MonthlyTokensPerTransaction > 0 && attempt.ReservedTokens > limiter.limits.MonthlyTokensPerTransaction-monthUsed {
		return "", Exceeded(LimitMonthlyTokens)
	}
	limiter.tokens[dayKey] = counter{window: day, value: dayUsed + attempt.ReservedTokens}
	limiter.tokens[monthKey] = counter{window: month, value: monthUsed + attempt.ReservedTokens}
	limiter.reservations[id] = memoryReservation{
		TokenReservation: TokenReservation{
			Version: 1, TransactionID: attempt.TransactionID, DeviceID: attempt.DeviceID,
			DailyWindow: day, MonthlyWindow: month, ReservedTokens: attempt.ReservedTokens,
			RootReservationID: attempt.ReservationID, Attempt: attempt.Number,
		},
		expiresAt: now.Add(25 * time.Hour),
	}
	return id, nil
}

var _ JobAttemptBudget = (*MemoryLimiter)(nil)
