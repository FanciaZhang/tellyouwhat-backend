package albums

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func checkMySQLReconciliationQueue(t *testing.T, ctx context.Context, db *sql.DB, u Upload, record MultipartCompletion, now time.Time) {
	t.Helper()
	r := NewMySQLUploads(db)
	job := ReconciliationJob{OwnerID: u.OwnerID, UploadID: u.ID, ResourceID: record.ResourceID, OperationID: record.OperationID, Kind: "completion"}
	checkReconciliationQueue(t, ctx, r, job, now)
}

func checkReconciliationQueue(t *testing.T, ctx context.Context, r *MySQLUploads, job ReconciliationJob, now time.Time) {
	t.Helper()
	db := r.db
	if _, err := r.ClaimReconciliation(ctx, job, now); !errors.Is(err, ErrLease) {
		t.Fatal("did not wait for active completion", err)
	}
	now = now.Add(3 * time.Minute)
	jobs, err := r.PendingReconciliations(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range jobs {
		if candidate.OperationID == job.OperationID {
			found = true
		}
	}
	if !found {
		t.Fatal("durable candidate missing")
	}
	type answer struct {
		job ReconciliationJob
		err error
	}
	results := make(chan answer, 20)
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			claimed, err := NewMySQLUploads(db).ClaimReconciliation(ctx, job, now)
			results <- answer{claimed, err}
		}()
	}
	group.Wait()
	close(results)
	var first ReconciliationJob
	wins := 0
	for value := range results {
		if value.err == nil {
			first = value.job
			wins++
		} else if !errors.Is(value.err, ErrLease) {
			t.Fatal(value.err)
		}
	}
	if wins != 1 || first.Attempt != 1 {
		t.Fatal("duplicate read permission", wins)
	}
	// Simulate a crash: another process can continue only after lease/backoff.
	if _, err := r.ClaimReconciliation(ctx, job, now.Add(16*time.Minute)); !errors.Is(err, ErrLease) {
		t.Fatal("crash backoff ignored", err)
	}
	now = now.Add(18 * time.Minute)
	second, err := NewMySQLUploads(db).ClaimReconciliation(ctx, job, now)
	if err != nil || second.Attempt != 2 || second.Token == first.Token {
		t.Fatal("crash recovery failed", err)
	}
	if err := r.FinishReconciliation(ctx, first, now); !errors.Is(err, ErrLease) {
		t.Fatal("stale worker released current lease", err)
	}
	wrong := second
	wrong.OwnerID = uuid.NewString()
	if err := r.FinishReconciliation(ctx, wrong, now); !errors.Is(err, ErrLease) {
		t.Fatal("cross account finish", err)
	}
	if err := r.FinishReconciliation(ctx, second, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimReconciliation(ctx, job, now.Add(4*time.Minute)); !errors.Is(err, ErrLease) {
		t.Fatal("retry backoff ignored", err)
	}
	now = now.Add(6 * time.Minute)
	third, err := r.ClaimReconciliation(ctx, job, now)
	if err != nil || third.Attempt != 3 {
		t.Fatal("third attempt", err)
	}
	if err := r.FinishReconciliation(ctx, third, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimReconciliation(ctx, job, now.Add(24*time.Hour)); !errors.Is(err, ErrLease) {
		t.Fatal("attempt budget reset", err)
	}
	jobs, err = r.PendingReconciliations(ctx, now.Add(24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range jobs {
		if candidate.OperationID == job.OperationID {
			t.Fatal("exhausted task still runnable")
		}
	}
}
