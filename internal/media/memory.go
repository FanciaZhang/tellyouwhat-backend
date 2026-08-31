package media

import (
	"context"
	"sync"
	"time"
)

type MemoryRegistry struct {
	mu       sync.RWMutex
	records  map[string]Record
	attempts map[string]AttemptRecord
}

func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{records: make(map[string]Record), attempts: make(map[string]AttemptRecord)}
}

func (registry *MemoryRegistry) Register(_ context.Context, record Record) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if existing, exists := registry.records[record.ObjectID]; exists {
		if !sameAuthorization(existing, record) || existing.DeletedAt != nil || existing.ConsumedAt != nil {
			return ErrAuthorizationConflict
		}
		return nil
	}
	registry.records[record.ObjectID] = record
	return nil
}

func (registry *MemoryRegistry) Get(_ context.Context, objectID string) (Record, error) {
	registry.mu.RLock()
	record, exists := registry.records[objectID]
	registry.mu.RUnlock()
	if !exists {
		return Record{}, ErrNotAuthorized
	}
	return record, nil
}

func (registry *MemoryRegistry) CommitAttempt(_ context.Context, expected []Record, attempt AttemptRecord, now time.Time) (AttemptRecord, bool, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if existing, exists := registry.attempts[attempt.RequestID]; exists && now.Before(existing.ExpiresAt) {
		if existing.OwnerKeyID == attempt.OwnerKeyID && existing.BodyDigest == attempt.BodyDigest {
			return existing, true, nil
		}
		return AttemptRecord{}, false, ErrIdempotencyConflict
	}
	seen := make(map[string]struct{}, len(expected))
	for _, authorization := range expected {
		record, exists := registry.records[authorization.ObjectID]
		if _, duplicate := seen[authorization.ObjectID]; duplicate || !exists || record.ConsumedAt != nil || record.DeletedAt != nil || !now.Before(record.ExpiresAt) || !sameAuthorization(record, authorization) {
			return AttemptRecord{}, false, ErrNotAuthorized
		}
		seen[authorization.ObjectID] = struct{}{}
	}
	consumedAt := now
	for _, authorization := range expected {
		record := registry.records[authorization.ObjectID]
		record.ConsumedAt = &consumedAt
		registry.records[authorization.ObjectID] = record
	}
	registry.attempts[attempt.RequestID] = attempt
	return attempt, false, nil
}

func sameAuthorization(left, right Record) bool {
	return left.ObjectID == right.ObjectID && left.OwnerKeyID == right.OwnerKeyID &&
		left.OwnerDeviceID == right.OwnerDeviceID && left.RequestID == right.RequestID &&
		left.Operation == right.Operation && left.MediaID == right.MediaID &&
		left.Kind == right.Kind && left.MIMEType == right.MIMEType && left.SHA256 == right.SHA256 &&
		left.SizeBytes == right.SizeBytes
}

