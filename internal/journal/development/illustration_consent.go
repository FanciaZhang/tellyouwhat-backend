package development

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/tellyouwhat/backend/internal/privacy"
)

// Preserve approved image access across developer-service restarts. Text/voice
// consent retains its existing lifecycle. Image withdrawal persists immediately.
type imageConsentRepository struct {
	*privacy.MemoryRepository
	mu      sync.Mutex
	runtime *IllustrationRuntime
	records map[string]privacy.Record
}

func newImageConsentRepository(runtime *IllustrationRuntime) (privacy.Repository, error) {
	repo := &imageConsentRepository{MemoryRepository: privacy.NewMemoryRepository(), runtime: runtime, records: map[string]privacy.Record{}}
	bytes, err := runtime.readEncrypted(filepath.Join(runtime.root, ".image-consents"))
	if errors.Is(err, os.ErrNotExist) {
		return repo, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(bytes, &repo.records); err != nil {
		return nil, err
	}
	var records []privacy.Record
	for _, record := range repo.records {
		if record.Scope != privacy.JournalIllustrationScope {
			return nil, privacy.ErrInvalidConsent
		}
		records = append(records, record)
	}
	if err = repo.MemoryRepository.RecordConsents(context.Background(), records); err != nil {
		return nil, err
	}
	return repo, nil
}
func (r *imageConsentRepository) RecordConsents(ctx context.Context, records []privacy.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]privacy.Record, len(r.records))
	for key, value := range r.records {
		next[key] = value
	}
	changed := false
	for _, record := range records {
		if record.Scope == privacy.JournalIllustrationScope {
			next[record.KeyID] = record
			changed = true
		}
	}
	if changed {
		bytes, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err = r.runtime.writeEncrypted(filepath.Join(r.runtime.root, ".image-consents"), bytes); err != nil {
			return err
		}
		r.records = next
	}
	return r.MemoryRepository.RecordConsents(ctx, records)
}
