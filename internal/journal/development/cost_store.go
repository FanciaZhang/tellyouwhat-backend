package development

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

// FileCostStore reuses the normal cost accounting implementation, with an atomic
// operation history so restarts and credential rotation do not reset spending.
// Only amounts and random attempt IDs are stored, never content or credentials.
type FileCostStore struct {
	mu     sync.Mutex
	path   string
	memory *costcontrol.MemoryStore
	events []costEvent
	failed error
}
type costEvent struct {
	Reserve *costcontrol.Attempt `json:"reserve,omitempty"`
	Budget  *monthlyBudgetChange `json:"budget,omitempty"`
	Limits  costcontrol.Limits   `json:"limits,omitempty"`
	ID      string               `json:"id,omitempty"`
	Actual  int64                `json:"actual,omitempty"`
	Known   bool                 `json:"known,omitempty"`
	Now     time.Time            `json:"now"`
}

type monthlyBudgetChange struct {
	MonthStart         time.Time `json:"month_start"`
	MonthlyBudgetNanos int64     `json:"monthly_budget_nanos"`
}

func NewFileCostStore(path string, configuredMonthlyBudgetNanos int64, now time.Time) (*FileCostStore, error) {
	if configuredMonthlyBudgetNanos <= 0 || now.IsZero() {
		return nil, costcontrol.ErrInvalidAttempt
	}
	s := &FileCostStore{path: path, memory: costcontrol.NewMemoryStore()}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if errors.Is(err, os.ErrNotExist) {
		if err = s.persist(); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err == nil {
		if err = json.Unmarshal(data, &s.events); err != nil {
			return nil, err
		}
		budgets, err := budgetHistory(s.events)
		if err != nil {
			return nil, err
		}
		changed := false
		monthStart := calendarMonth(now)
		month := monthKey(monthStart)
		if existing, exists := budgets[month]; exists {
			if configuredMonthlyBudgetNanos < existing {
				return nil, costcontrol.ErrConfigurationConflict
			}
			if configuredMonthlyBudgetNanos > existing {
				s.events = append(s.events, costEvent{
					Budget: &monthlyBudgetChange{
						MonthStart:         monthStart,
						MonthlyBudgetNanos: configuredMonthlyBudgetNanos,
					},
					Now: now.UTC(),
				})
				budgets[month] = configuredMonthlyBudgetNanos
				changed = true
			}
		}
		pending := map[string]bool{}
		for _, e := range s.events {
			if err = s.applyReplay(context.Background(), e, budgets); err != nil {
				return nil, err
			}
			if e.Reserve != nil {
				pending[e.Reserve.ID] = true
			} else if e.Budget == nil {
				delete(pending, e.ID)
			}
		}
		// This store belongs to the single private service process. After a
		// restart none of its old calls can still run. Keep their reservations
		// charged as unknown, but release the dead concurrency slots immediately.
		if len(pending) > 0 {
			for id := range pending {
				e := costEvent{ID: id, Now: time.Now()}
				if err = s.apply(context.Background(), e); err != nil {
					return nil, err
				}
				s.events = append(s.events, e)
			}
			changed = true
		}
		if changed {
			if err = s.persist(); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func budgetHistory(events []costEvent) (map[string]int64, error) {
	budgets := map[string]int64{}
	for _, e := range events {
		if e.Reserve != nil {
			month := monthKey(e.Reserve.MonthStart)
			if month == "" || e.Limits.MonthlyBudgetNanos <= 0 {
				return nil, costcontrol.ErrInvalidAttempt
			}
			if existing, exists := budgets[month]; exists && existing != e.Limits.MonthlyBudgetNanos {
				return nil, costcontrol.ErrConfigurationConflict
			}
			budgets[month] = e.Limits.MonthlyBudgetNanos
			continue
		}
		if e.Budget == nil {
			continue
		}
		month := monthKey(e.Budget.MonthStart)
		if month == "" || !e.Budget.MonthStart.Equal(calendarMonth(e.Budget.MonthStart)) || e.Budget.MonthlyBudgetNanos <= 0 {
			return nil, costcontrol.ErrInvalidAttempt
		}
		if existing, exists := budgets[month]; exists && e.Budget.MonthlyBudgetNanos <= existing {
			return nil, costcontrol.ErrConfigurationConflict
		}
		budgets[month] = e.Budget.MonthlyBudgetNanos
	}
	return budgets, nil
}

func calendarMonth(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func monthKey(monthStart time.Time) string {
	if monthStart.IsZero() {
		return ""
	}
	return calendarMonth(monthStart).Format("2006-01-02")
}

func (s *FileCostStore) applyReplay(ctx context.Context, e costEvent, budgets map[string]int64) error {
	if e.Reserve != nil {
		limits := e.Limits
		if budget, exists := budgets[monthKey(e.Reserve.MonthStart)]; exists {
			limits.MonthlyBudgetNanos = budget
		}
		return s.memory.Reserve(ctx, *e.Reserve, limits)
	}
	if e.Budget != nil {
		return nil
	}
	return s.memory.Settle(ctx, e.ID, e.Actual, e.Known, e.Now)
}

func (s *FileCostStore) apply(ctx context.Context, e costEvent) error {
	if e.Reserve != nil {
		return s.memory.Reserve(ctx, *e.Reserve, e.Limits)
	}
	if e.Budget != nil {
		return nil
	}
	return s.memory.Settle(ctx, e.ID, e.Actual, e.Known, e.Now)
}
func (s *FileCostStore) write(ctx context.Context, e costEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return s.failed
	}
	if err := s.apply(ctx, e); err != nil {
		return err
	}
	s.events = append(s.events, e)
	// A failed persistence write closes the store to subsequent spending. On
	// restart, unsettled reservations remain conservatively charged.
	s.failed = s.persist()
	return s.failed
}
func (s *FileCostStore) persist() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".cost-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(s.events); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *FileCostStore) Reserve(ctx context.Context, a costcontrol.Attempt, l costcontrol.Limits) error {
	return s.write(ctx, costEvent{Reserve: &a, Limits: l, Now: a.CreatedAt})
}
func (s *FileCostStore) Settle(ctx context.Context, id string, actual int64, known bool, now time.Time) error {
	return s.write(ctx, costEvent{ID: id, Actual: actual, Known: known, Now: now})
}
