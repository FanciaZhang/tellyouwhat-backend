package capability

import (
	"context"
	"sync"
	"time"
)

type MemoryUseStore struct {
	mu   sync.Mutex
	used map[string]time.Time
}

func NewMemoryUseStore() *MemoryUseStore {
	return &MemoryUseStore{used: make(map[string]time.Time)}
}

func (store *MemoryUseStore) Consume(_ context.Context, nonce string, expiresAt, now time.Time) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for key, expiry := range store.used {
		if !now.Before(expiry) {
			delete(store.used, key)
		}
	}
	if _, exists := store.used[nonce]; exists {
		return false, nil
	}
	store.used[nonce] = expiresAt
	return true, nil
}
