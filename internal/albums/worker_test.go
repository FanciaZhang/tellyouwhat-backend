package albums

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

func (r *uploadTestRepository) Pending(ctx context.Context, now time.Time, limit int) ([]Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var jobs []Upload
	for _, u := range r.values {
		if now.Before(u.ExpiresAt) && (u.State == Queued || (u.State == Verifying && !now.Before(u.LeaseUntil))) {
			jobs = append(jobs, u)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Manifest.AssetID < jobs[j].Manifest.AssetID })
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

func TestVerificationWorkerContinuesAfterFailureAndResumesExpiredLease(t *testing.T) {
	s, repo, objects, owner, manifest := uploadFixture(t)
	now := s.now()
	s.now = func() time.Time { return now }
	ctx := context.Background()
	var jobs []Upload
	for _, id := range []string{"a-unavailable", "b-ready"} {
		m := manifest
		m.AssetID = id
		u, err := s.Create(ctx, owner, uuid.NewString(), m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Submit(ctx, owner, u.ID); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, u)
	}
	objects.staging[stagingKey(jobs[1], "photo")] = []byte("original-resource-bytes")
	worker, err := NewVerificationWorker(repo, s, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.RunOnce(ctx)
	if err != nil || result.Verified != 1 || result.Failed != 1 {
		t.Fatalf("batch: %+v %v", result, err)
	}
	if repo.completed != 1 {
		t.Fatal("failed object became verified")
	}
	result, err = worker.RunOnce(ctx)
	if err != nil || result != (VerificationBatch{}) {
		t.Fatalf("live lease was retried: %+v %v", result, err)
	}
	objects.staging[stagingKey(jobs[0], "photo")] = []byte("original-resource-bytes")
	now = now.Add(s.policy.VerificationLease + time.Second)
	// A fresh worker represents process restart; all remaining work is in the repository.
	restarted, _ := NewVerificationWorker(repo, s, 2, time.Second)
	result, err = restarted.RunOnce(ctx)
	if err != nil || result.Verified != 1 || repo.completed != 2 {
		t.Fatalf("resume: %+v %v", result, err)
	}
}

func TestVerificationWorkerCancellationStopsPolling(t *testing.T) {
	s, repo, _, _, _ := uploadFixture(t)
	worker, _ := NewVerificationWorker(repo, s, 1, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := worker.Run(ctx, func(result VerificationBatch, available bool) {
		calls++
		if !available || result != (VerificationBatch{}) {
			t.Error("unexpected empty queue outcome")
		}
		cancel()
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("shutdown: %v calls=%d", err, calls)
	}
	result, err := worker.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) || result != (VerificationBatch{}) {
		t.Fatalf("cancelled batch: %+v %v", result, err)
	}
}
