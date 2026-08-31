package adminauth

import (
	"context"
	"sync"
	"time"
)

type expiringValue[T any] struct {
	value     T
	expiresAt time.Time
}

type MemoryStateStore struct {
	mutex      sync.Mutex
	ceremonies map[string]expiringValue[CeremonyState]
	sessions   map[[32]byte]expiringValue[Session]
	now        func() time.Time
}

func NewMemoryStateStore(now func() time.Time) *MemoryStateStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryStateStore{
		ceremonies: make(map[string]expiringValue[CeremonyState]),
		sessions:   make(map[[32]byte]expiringValue[Session]),
		now:        now,
	}
}

func (store *MemoryStateStore) PutCeremony(
	_ context.Context,
	id string,
	state CeremonyState,
	ttl time.Duration,
) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.ceremonies[id] = expiringValue[CeremonyState]{state, store.now().Add(ttl)}
	return nil
}

func (store *MemoryStateStore) TakeCeremony(
	_ context.Context,
	id string,
) (CeremonyState, bool, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	value, found := store.ceremonies[id]
	delete(store.ceremonies, id)
	if !found || !store.now().Before(value.expiresAt) {
		return CeremonyState{}, false, nil
	}
	return value.value, true, nil
}

func (store *MemoryStateStore) PutSession(
	_ context.Context,
	hash [32]byte,
	session Session,
	ttl time.Duration,
) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.sessions[hash] = expiringValue[Session]{session, store.now().Add(ttl)}
	return nil
}

func (store *MemoryStateStore) GetSession(
	_ context.Context,
	hash [32]byte,
) (Session, bool, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	value, found := store.sessions[hash]
	if !found || !store.now().Before(value.expiresAt) {
		delete(store.sessions, hash)
		return Session{}, false, nil
	}
	return value.value, true, nil
}

func (store *MemoryStateStore) DeleteSession(_ context.Context, hash [32]byte) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	delete(store.sessions, hash)
	return nil
}

var _ StateStore = (*MemoryStateStore)(nil)
