package attestation

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

type nonceEntry struct {
	keyID     string
	expiresAt time.Time
	used      bool
}

type MemoryNonceStore struct {
	mu      sync.Mutex
	entries map[string]nonceEntry
}

func NewMemoryNonceStore() *MemoryNonceStore {
	return &MemoryNonceStore{entries: make(map[string]nonceEntry)}
}

func (store *MemoryNonceStore) Issue(
	_ context.Context,
	keyID string,
	ttl time.Duration,
	now time.Time,
) (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	nonce := base64.RawURLEncoding.EncodeToString(random)
	store.mu.Lock()
	store.entries[nonce] = nonceEntry{keyID: keyID, expiresAt: now.Add(ttl)}
	store.mu.Unlock()
	return nonce, nil
}

func (store *MemoryNonceStore) Consume(
	_ context.Context,
	nonce string,
	keyID string,
	now time.Time,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.entries[nonce]
	if !ok || entry.keyID != keyID || !now.Before(entry.expiresAt) {
		return ErrAuthentication
	}
	if entry.used {
		return ErrReplay
	}
	entry.used = true
	store.entries[nonce] = entry
	return nil
}

type MemoryKeyStore struct {
	mu   sync.RWMutex
	keys map[string]RegisteredKey
}

func NewMemoryKeyStore() *MemoryKeyStore {
	return &MemoryKeyStore{keys: make(map[string]RegisteredKey)}
}

func (store *MemoryKeyStore) Put(key RegisteredKey) {
	store.mu.Lock()
	key.PublicKey = append([]byte(nil), key.PublicKey...)
	store.keys[key.KeyID] = key
	store.mu.Unlock()
}

func (store *MemoryKeyStore) Register(_ context.Context, key RegisteredKey) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.keys[key.KeyID]; exists {
		return ErrKeyAlreadyRegistered
	}
	key.PublicKey = append([]byte(nil), key.PublicKey...)
	store.keys[key.KeyID] = key
	return nil
}

func (store *MemoryKeyStore) Get(_ context.Context, keyID string) (RegisteredKey, error) {
	store.mu.RLock()
	key, ok := store.keys[keyID]
	store.mu.RUnlock()
	if !ok {
		return RegisteredKey{}, ErrKeyNotFound
	}
	key.PublicKey = append([]byte(nil), key.PublicKey...)
	return key, nil
}

func (store *MemoryKeyStore) AdvanceCounter(
	_ context.Context,
	keyID string,
	expected uint32,
	next uint32,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	key, ok := store.keys[keyID]
	if !ok {
		return ErrKeyNotFound
	}
	if key.Counter != expected || next <= expected {
		return ErrReplay
	}
	key.Counter = next
	store.keys[keyID] = key
	return nil
}

func (store *MemoryKeyStore) BindTransaction(
	_ context.Context,
	keyID string,
	transactionID string,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	key, ok := store.keys[keyID]
	if !ok {
		return ErrKeyNotFound
	}
	if key.TransactionID != "" && key.TransactionID != transactionID {
		return ErrTransactionBindingConflict
	}
	key.TransactionID = transactionID
	store.keys[keyID] = key
	return nil
}
