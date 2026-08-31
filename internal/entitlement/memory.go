package entitlement

import (
	"context"
	"sync"
)

type MemoryStore struct {
	mu            sync.RWMutex
	records       map[string]Record
	notifications map[string]struct{}
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		records:       make(map[string]Record),
		notifications: make(map[string]struct{}),
	}
}

func (store *MemoryStore) Upsert(_ context.Context, record Record) error {
	store.mu.Lock()
	store.records[record.KeyID] = record
	store.mu.Unlock()
	return nil
}

func (store *MemoryStore) Get(_ context.Context, keyID string) (Record, bool, error) {
	store.mu.RLock()
	record, ok := store.records[keyID]
	store.mu.RUnlock()
	return record, ok, nil
}

func (store *MemoryStore) ApplyNotification(
	_ context.Context,
	state NotificationState,
) (bool, error) {
	if state.NotificationUUID == "" || state.OriginalTransactionID == "" {
		return false, ErrProductionSyncDenied
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.notifications[state.NotificationUUID]; exists {
		return false, nil
	}
	store.notifications[state.NotificationUUID] = struct{}{}
	for keyID, record := range store.records {
		if record.TransactionID != state.OriginalTransactionID {
			continue
		}
		record.Environment = state.Environment
		record.ExpiresAt = state.ExpiresAt
		store.records[keyID] = record
	}
	return true, nil
}
