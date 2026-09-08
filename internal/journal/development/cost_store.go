package development

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	Limits  costcontrol.Limits   `json:"limits,omitempty"`
	ID      string               `json:"id,omitempty"`
	Actual  int64                `json:"actual,omitempty"`
	Known   bool                 `json:"known,omitempty"`
	Now     time.Time            `json:"now"`
}

func NewFileCostStore(path string) (*FileCostStore, error) {
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
		pending := map[string]bool{}
		for _, e := range s.events {
			if err = s.apply(context.Background(), e); err != nil {
				return nil, err
			}
			if e.Reserve != nil {
				pending[e.Reserve.ID] = true
			} else {
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
			if err = s.persist(); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}
func (s *FileCostStore) apply(ctx context.Context, e costEvent) error {
	if e.Reserve != nil {
		return s.memory.Reserve(ctx, *e.Reserve, e.Limits)
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
