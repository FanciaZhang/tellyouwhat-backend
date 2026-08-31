package privacy

import (
	"context"
	"sync"

	"github.com/tellyouwhat/backend/internal/attestation"
)

type MemoryRepository struct {
	mu      sync.Mutex
	records map[string]Record
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{records: make(map[string]Record)}
}

func (repository *MemoryRepository) RecordConsents(_ context.Context, records []Record) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	for _, record := range records {
		repository.records[record.KeyID+"\x00"+record.Scope+"\x00"+record.DocumentVersion] = record
	}
	return nil
}

func (*MemoryRepository) PlanDeletion(_ context.Context, principal attestation.Principal) (DeletionPlan, error) {
	return DeletionPlan{Principals: []attestation.Principal{principal}}, nil
}

func (repository *MemoryRepository) DeletePrincipal(_ context.Context, principal attestation.Principal) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	for key, record := range repository.records {
		if record.KeyID == principal.KeyID {
			delete(repository.records, key)
		}
	}
	return nil
}
