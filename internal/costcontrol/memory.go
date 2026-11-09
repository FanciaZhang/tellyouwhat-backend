package costcontrol

import (
	"context"
	"sync"
	"time"
)

type memoryAttempt struct {
	Attempt
	status string
	actual int64
}

type MemoryStore struct {
	mu       sync.Mutex
	budgets  map[string]int64
	charged  map[string]int64
	attempts map[string]*memoryAttempt
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{budgets: map[string]int64{}, charged: map[string]int64{}, attempts: map[string]*memoryAttempt{}}
}

func (store *MemoryStore) Reserve(_ context.Context, attempt Attempt, limits Limits) error {
	if store == nil || !attempt.Valid() || !limits.Valid() {
		return ErrInvalidAttempt
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, existing := range store.attempts {
		if existing.status == "pending" && !attempt.CreatedAt.Before(existing.LeaseExpiresAt) {
			existing.status = "unknown"
		}
	}
	concurrent := 0
	for _, existing := range store.attempts {
		if existing.status == "pending" {
			concurrent++
		}
	}
	if concurrent >= limits.MaxConcurrent {
		return ErrConcurrencyExceeded
	}
	month := attempt.MonthStart.Format("2006-01-02")
	if budget, exists := store.budgets[month]; exists && budget != limits.MonthlyBudgetNanos {
		return ErrConfigurationConflict
	}
	store.budgets[month] = limits.MonthlyBudgetNanos
	used := store.charged[month]
	if attempt.ReservedNanos > limits.MonthlyBudgetNanos-used {
		return ErrBudgetExceeded
	}
	if _, exists := store.attempts[attempt.ID]; exists {
		return ErrInvalidAttempt
	}
	store.charged[month] = used + attempt.ReservedNanos
	store.attempts[attempt.ID] = &memoryAttempt{Attempt: attempt, status: "pending"}
	return nil
}

func (store *MemoryStore) Settle(_ context.Context, attemptID string, actualNanos int64, known bool, _ time.Time) error {
	if store == nil || attemptID == "" || actualNanos < 0 {
		return ErrInvalidAttempt
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	attempt, ok := store.attempts[attemptID]
	if !ok {
		return ErrInvalidAttempt
	}
	if attempt.status != "pending" {
		return nil
	}
	if !known {
		attempt.status = "unknown"
		return nil
	}
	month := attempt.MonthStart.Format("2006-01-02")
	if actualNanos > int64(^uint64(0)>>1)-(store.charged[month]-attempt.ReservedNanos) {
		return ErrInvalidAttempt
	}
	store.charged[month] = store.charged[month] - attempt.ReservedNanos + actualNanos
	attempt.actual = actualNanos
	attempt.status = "settled"
	return nil
}

var _ Store = (*MemoryStore)(nil)
