package quota

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
)

var (
	ErrExceeded        = errors.New("quota exceeded")
	ErrInvalidIdentity = errors.New("invalid quota identity")
)

type LimitScope string

const (
	LimitRequestsPerMinuteIP        LimitScope = "requests_per_minute_ip"
	LimitRequestsPerMinuteDevice    LimitScope = "requests_per_minute_device"
	LimitRequestsPerMinuteOperation LimitScope = "requests_per_minute_operation"
	LimitDailyTokens                LimitScope = "daily_tokens"
	LimitMonthlyTokens              LimitScope = "monthly_tokens"
	LimitConcurrentDevice           LimitScope = "concurrent_device"
)

type LimitExceededError struct {
	Scope LimitScope
}

func (err LimitExceededError) Error() string {
	return "quota exceeded: " + string(err.Scope)
}

func (err LimitExceededError) Is(target error) bool {
	return target == ErrExceeded
}

func Exceeded(scope LimitScope) error {
	return LimitExceededError{Scope: scope}
}

func ExceededScope(err error) (LimitScope, bool) {
	var exceeded LimitExceededError
	if errors.As(err, &exceeded) {
		return exceeded.Scope, true
	}
	return "", false
}

type Limits struct {
	RequestsPerMinutePerIP        int
	RequestsPerMinutePerDevice    int
	RequestsPerMinutePerOperation int
	DailyTokensPerTransaction     int
	MonthlyTokensPerTransaction   int
	MaxConcurrentPerDevice        int
}

type Identity struct {
	DeviceID      string
	TransactionID string
	IP            string
}

type Snapshot struct {
	DailyUsed      int       `json:"dailyUsed"`
	DailyLimit     int       `json:"dailyLimit"`
	DailyResetAt   time.Time `json:"dailyResetAt"`
	MonthlyUsed    int       `json:"monthlyUsed"`
	MonthlyLimit   int       `json:"monthlyLimit"`
	MonthlyResetAt time.Time `json:"monthlyResetAt"`
}

type Reader interface {
	Snapshot(context.Context, string, time.Time) (Snapshot, error)
}

type counter struct {
	window string
	value  int
}

type MemoryLimiter struct {
	mu           sync.Mutex
	limits       Limits
	requests     map[string]counter
	tokens       map[string]counter
	concurrency  map[string]int
	reservations map[string]time.Time
}

func NewMemoryLimiter(limits Limits) *MemoryLimiter {
	return &MemoryLimiter{
		limits:       limits,
		requests:     make(map[string]counter),
		tokens:       make(map[string]counter),
		concurrency:  make(map[string]int),
		reservations: make(map[string]time.Time),
	}
}

type Lease struct {
	once          sync.Once
	limiter       *MemoryLimiter
	deviceKey     string
	dailyKey      string
	monthlyKey    string
	dailyWindow   string
	monthlyWindow string
	reserved      int
	reservationID string
	owned         bool
}

type Releaser interface {
	Release(actualTokens int)
}

type TokenReconciler interface {
	Reconcile(context.Context, string, int, int, time.Time) error
}

func (limiter *MemoryLimiter) Acquire(
	_ context.Context,
	identity Identity,
	operation contracts.Operation,
	estimatedTokens int,
	reservationID string,
	now time.Time,
) (Releaser, error) {
	if limiter == nil || identity.DeviceID == "" || identity.TransactionID == "" || identity.IP == "" || estimatedTokens < 0 {
		return nil, ErrInvalidIdentity
	}
	minuteWindow := now.UTC().Format("200601021504")
	dailyWindow := now.UTC().Format("20060102")
	monthlyWindow := now.UTC().Format("200601")
	requestDimensions := []struct {
		key   string
		limit int
		scope LimitScope
	}{
		{key: "ip:" + identity.IP, limit: limiter.limits.RequestsPerMinutePerIP, scope: LimitRequestsPerMinuteIP},
		{key: "device:" + identity.DeviceID, limit: limiter.limits.RequestsPerMinutePerDevice, scope: LimitRequestsPerMinuteDevice},
		{key: "operation:" + identity.TransactionID + ":" + string(operation), limit: limiter.limits.RequestsPerMinutePerOperation, scope: LimitRequestsPerMinuteOperation},
	}
	dailyKey := "day:" + identity.TransactionID
	monthlyKey := "month:" + identity.TransactionID

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if reservationID != "" {
		if expiresAt, exists := limiter.reservations[reservationID]; exists && now.Before(expiresAt) {
			return &Lease{}, nil
		}
		delete(limiter.reservations, reservationID)
	}
	for _, dimension := range requestDimensions {
		if dimension.limit > 0 && counterValue(limiter.requests[dimension.key], minuteWindow)+1 > dimension.limit {
			return nil, Exceeded(dimension.scope)
		}
	}
	if limiter.limits.MaxConcurrentPerDevice > 0 && limiter.concurrency[identity.DeviceID]+1 > limiter.limits.MaxConcurrentPerDevice {
		return nil, Exceeded(LimitConcurrentDevice)
	}
	if limiter.limits.DailyTokensPerTransaction > 0 && counterValue(limiter.tokens[dailyKey], dailyWindow)+estimatedTokens > limiter.limits.DailyTokensPerTransaction {
		return nil, Exceeded(LimitDailyTokens)
	}
	if limiter.limits.MonthlyTokensPerTransaction > 0 && counterValue(limiter.tokens[monthlyKey], monthlyWindow)+estimatedTokens > limiter.limits.MonthlyTokensPerTransaction {
		return nil, Exceeded(LimitMonthlyTokens)
	}
	for _, dimension := range requestDimensions {
		limiter.requests[dimension.key] = counter{window: minuteWindow, value: counterValue(limiter.requests[dimension.key], minuteWindow) + 1}
	}
	limiter.tokens[dailyKey] = counter{window: dailyWindow, value: counterValue(limiter.tokens[dailyKey], dailyWindow) + estimatedTokens}
	limiter.tokens[monthlyKey] = counter{window: monthlyWindow, value: counterValue(limiter.tokens[monthlyKey], monthlyWindow) + estimatedTokens}
	limiter.concurrency[identity.DeviceID]++
	if reservationID != "" {
		limiter.reservations[reservationID] = now.Add(25 * time.Hour)
	}
	return &Lease{
		limiter:       limiter,
		deviceKey:     identity.DeviceID,
		dailyKey:      dailyKey,
		monthlyKey:    monthlyKey,
		dailyWindow:   dailyWindow,
		monthlyWindow: monthlyWindow,
		reserved:      estimatedTokens,
		reservationID: reservationID,
		owned:         true,
	}, nil
}

func (lease *Lease) Release(actualTokens int) {
	if lease == nil || lease.limiter == nil || !lease.owned {
		return
	}
	lease.once.Do(func() {
		if actualTokens < 0 {
			actualTokens = 0
		}
		delta := actualTokens - lease.reserved
		lease.limiter.mu.Lock()
		lease.limiter.concurrency[lease.deviceKey]--
		if lease.limiter.concurrency[lease.deviceKey] <= 0 {
			delete(lease.limiter.concurrency, lease.deviceKey)
		}
		adjustCounter(lease.limiter.tokens, lease.dailyKey, lease.dailyWindow, delta)
		adjustCounter(lease.limiter.tokens, lease.monthlyKey, lease.monthlyWindow, delta)
		if actualTokens == 0 && lease.reservationID != "" {
			delete(lease.limiter.reservations, lease.reservationID)
		}
		lease.limiter.mu.Unlock()
	})
}

func (limiter *MemoryLimiter) Reconcile(
	_ context.Context,
	transactionID string,
	reserved,
	actual int,
	now time.Time,
) error {
	if limiter == nil || transactionID == "" || reserved < 0 || actual < 0 {
		return ErrInvalidIdentity
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	delta := actual - reserved
	adjustCounter(limiter.tokens, "day:"+transactionID, now.UTC().Format("20060102"), delta)
	adjustCounter(limiter.tokens, "month:"+transactionID, now.UTC().Format("200601"), delta)
	return nil
}

func (limiter *MemoryLimiter) Snapshot(
	_ context.Context,
	transactionID string,
	now time.Time,
) (Snapshot, error) {
	if limiter == nil || transactionID == "" {
		return Snapshot{}, ErrInvalidIdentity
	}
	utc := now.UTC()
	dailyWindow := utc.Format("20060102")
	monthlyWindow := utc.Format("200601")
	limiter.mu.Lock()
	dailyUsed := counterValue(limiter.tokens["day:"+transactionID], dailyWindow)
	monthlyUsed := counterValue(limiter.tokens["month:"+transactionID], monthlyWindow)
	limiter.mu.Unlock()
	return Snapshot{
		DailyUsed:      dailyUsed,
		DailyLimit:     limiter.limits.DailyTokensPerTransaction,
		DailyResetAt:   utc.Truncate(24 * time.Hour).Add(24 * time.Hour),
		MonthlyUsed:    monthlyUsed,
		MonthlyLimit:   limiter.limits.MonthlyTokensPerTransaction,
		MonthlyResetAt: time.Date(utc.Year(), utc.Month()+1, 1, 0, 0, 0, 0, time.UTC),
	}, nil
}

func counterValue(value counter, window string) int {
	if value.window != window {
		return 0
	}
	return value.value
}

func adjustCounter(values map[string]counter, key, window string, delta int) {
	value := counterValue(values[key], window) + delta
	if value < 0 {
		value = 0
	}
	values[key] = counter{window: window, value: value}
}
