package jobs

import (
	"context"
	"sync"
	"time"

	providerapi "github.com/tellyouwhat/backend/internal/provider"
	"github.com/tellyouwhat/backend/internal/usage"
)

type MemoryStore struct {
	mu          sync.RWMutex
	jobs        map[string]Job
	byRequestID map[string]string
	outbox      map[string]DispatchItem
	usage       map[string]usage.Record
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		jobs:        make(map[string]Job),
		byRequestID: make(map[string]string),
		outbox:      make(map[string]DispatchItem),
		usage:       make(map[string]usage.Record),
	}
}

func (store *MemoryStore) CreateOrGet(_ context.Context, job Job) (Job, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if existingID, ok := store.byRequestID[job.RequestID]; ok {
		existing := store.jobs[existingID]
		if existing.BodyDigest != job.BodyDigest || existing.OwnerKeyID != job.OwnerKeyID {
			return Job{}, ErrIdempotencyConflict
		}
		return existing, nil
	}
	store.jobs[job.ID] = job
	store.byRequestID[job.RequestID] = job.ID
	store.outbox[job.ID] = DispatchItem{JobID: job.ID}
	return job, nil
}

func (store *MemoryStore) Get(_ context.Context, jobID string) (Job, error) {
	store.mu.RLock()
	job, ok := store.jobs[jobID]
	store.mu.RUnlock()
	if !ok {
		return Job{}, ErrNotFound
	}
	return job, nil
}

func (store *MemoryStore) Claim(_ context.Context, jobID string, now time.Time) (Job, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, ok := store.jobs[jobID]
	if !ok {
		return Job{}, ErrNotFound
	}
	reclaimable := job.Status == StatusRunning && !now.Before(job.ClaimExpiresAt)
	if (job.Status != StatusQueued && !reclaimable) || !now.Before(job.ExpiresAt) || job.AttemptCount >= maximumAttempts {
		return Job{}, ErrJobNotClaimable
	}
	job.Status = StatusRunning
	job.AttemptCount++
	job.ClaimExpiresAt = now.Add(claimLeaseDuration)
	job.UpdatedAt = now
	store.jobs[jobID] = job
	return job, nil
}

func (store *MemoryStore) ExtendLease(_ context.Context, jobID string, attempt int, now time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, exists := store.jobs[jobID]
	if !exists {
		return ErrNotFound
	}
	if job.Status != StatusRunning || job.AttemptCount != attempt {
		return ErrJobNotClaimable
	}
	job.ClaimExpiresAt = now.Add(claimLeaseDuration)
	job.UpdatedAt = now
	store.jobs[jobID] = job
	return nil
}

func (store *MemoryStore) Succeed(_ context.Context, jobID string, attempt int, response providerapi.Response, record usage.Record, now time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, ok := store.jobs[jobID]
	if !ok {
		return ErrNotFound
	}
	if job.Status != StatusRunning || job.AttemptCount != attempt {
		return ErrJobNotClaimable
	}
	if record.RequestID != job.RequestID || record.KeyID != job.OwnerKeyID {
		return ErrIdempotencyConflict
	}
	job.Status = StatusSucceeded
	job.Result = response.Content
	job.InputTokens = response.InputTokens
	job.OutputTokens = response.OutputTokens
	job.UpdatedAt = now
	job.ClaimExpiresAt = time.Time{}
	store.jobs[jobID] = job
	store.usage[record.RequestID] = record
	delete(store.outbox, jobID)
	return nil
}

func (store *MemoryStore) RetryOrFail(_ context.Context, jobID string, attempt int, category string, now time.Time) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, ok := store.jobs[jobID]
	if !ok {
		return false, ErrNotFound
	}
	if job.Status != StatusRunning || job.AttemptCount != attempt {
		return false, ErrJobNotClaimable
	}
	job.FailureCategory = category
	job.UpdatedAt = now
	job.ClaimExpiresAt = time.Time{}
	if job.AttemptCount >= maximumAttempts {
		job.Status = StatusFailed
		delete(store.outbox, jobID)
		store.jobs[jobID] = job
		return false, nil
	}
	job.Status = StatusQueued
	store.jobs[jobID] = job
	store.outbox[jobID] = DispatchItem{JobID: jobID}
	return true, nil
}

func (store *MemoryStore) Fail(_ context.Context, jobID string, attempt int, category string, now time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, ok := store.jobs[jobID]
	if !ok {
		return ErrNotFound
	}
	if job.Status == StatusCancelled {
		return nil
	}
	if job.Status != StatusRunning || job.AttemptCount != attempt {
		return ErrJobNotClaimable
	}
	job.Status = StatusFailed
	job.FailureCategory = category
	job.UpdatedAt = now
	job.ClaimExpiresAt = time.Time{}
	store.jobs[jobID] = job
	delete(store.outbox, jobID)
	return nil
}

func (store *MemoryStore) Cancel(_ context.Context, jobID string, now time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, ok := store.jobs[jobID]
	if !ok {
		return ErrNotFound
	}
	if job.Status == StatusSucceeded || job.Status == StatusFailed {
		return ErrJobNotClaimable
	}
	job.Status = StatusCancelled
	job.UpdatedAt = now
	job.ClaimExpiresAt = time.Time{}
	store.jobs[jobID] = job
	delete(store.outbox, jobID)
	return nil
}

func (store *MemoryStore) ClaimDispatches(_ context.Context, now time.Time, limit int) ([]DispatchItem, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	items := make([]DispatchItem, 0, limit)
	for jobID, item := range store.outbox {
		job := store.jobs[jobID]
		if !now.Before(job.ExpiresAt) {
			job.Status = StatusFailed
			job.FailureCategory = "job_expired"
			job.UpdatedAt = now
			job.ClaimExpiresAt = time.Time{}
			store.jobs[jobID] = job
			delete(store.outbox, jobID)
			continue
		}
		jobReady := job.Status == StatusQueued || (job.Status == StatusRunning && !now.Before(job.ClaimExpiresAt))
		if jobReady && (job.AttemptCount >= maximumAttempts || item.Attempts >= 10) {
			job.Status = StatusFailed
			if job.AttemptCount >= maximumAttempts {
				job.FailureCategory = "worker_attempts_exhausted"
			} else {
				job.FailureCategory = "worker_delivery_exhausted"
			}
			job.UpdatedAt = now
			job.ClaimExpiresAt = time.Time{}
			store.jobs[jobID] = job
			delete(store.outbox, jobID)
			continue
		}
		if len(items) >= limit || !jobReady || now.Before(item.AvailableAt) || now.Before(item.ClaimedUntil) {
			continue
		}
		item.Attempts++
		item.ClaimedUntil = now.Add(30 * time.Second)
		store.outbox[jobID] = item
		items = append(items, item)
	}
	return items, nil
}

func (store *MemoryStore) CompleteDispatch(_ context.Context, jobID string, now time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	item, exists := store.outbox[jobID]
	if !exists {
		return nil
	}
	item.AvailableAt = now.Add(claimLeaseDuration)
	item.ClaimedUntil = time.Time{}
	store.outbox[jobID] = item
	return nil
}

func (store *MemoryStore) RetryDispatch(_ context.Context, jobID string, now time.Time, category string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	item, exists := store.outbox[jobID]
	if !exists {
		return nil
	}
	if item.Attempts >= 10 {
		job := store.jobs[jobID]
		ready := job.Status == StatusQueued || (job.Status == StatusRunning && !now.Before(job.ClaimExpiresAt))
		if ready {
			job.Status = StatusFailed
			job.FailureCategory = category
			job.UpdatedAt = now
			job.ClaimExpiresAt = time.Time{}
			store.jobs[jobID] = job
			delete(store.outbox, jobID)
		} else {
			item.AvailableAt = now.Add(claimLeaseDuration)
			item.ClaimedUntil = time.Time{}
			store.outbox[jobID] = item
		}
		return nil
	}
	backoff := time.Duration(1<<min(item.Attempts, 6)) * time.Second
	item.AvailableAt = now.Add(backoff)
	item.ClaimedUntil = time.Time{}
	store.outbox[jobID] = item
	return nil
}

var _ OutboxStore = (*MemoryStore)(nil)
