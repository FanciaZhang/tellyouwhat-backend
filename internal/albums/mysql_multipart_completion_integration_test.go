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

func checkMySQLMultipartCompletion(t *testing.T, ctx context.Context, db *sql.DB, u Upload, now time.Time) {
	t.Helper()
	resource := u.Manifest.Resources[0].ID
	r := NewMySQLUploads(db)
	type answer struct {
		value   MultipartCompletion
		claimed bool
		err     error
	}
	results := make(chan answer, 20)
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			value, claimed, err := NewMySQLUploads(db).ClaimMultipartCompletion(ctx, u.OwnerID, u.ID, resource, now)
			results <- answer{value, claimed, err}
		}()
	}
	group.Wait()
	close(results)
	var first MultipartCompletion
	winners := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if first.OperationID == "" {
			first = result.value
		}
		if result.value != first {
			t.Fatal("concurrent completions differ")
		}
		if result.claimed {
			winners++
		}
	}
	stored, readErr := r.GetMultipartCompletion(ctx, u.OwnerID, u.ID, resource)
	if readErr != nil || stored != first {
		t.Fatal("completion read lost durable state", readErr)
	}
	if _, readErr := r.GetMultipartCompletion(ctx, uuid.NewString(), u.ID, resource); !errors.Is(readErr, ErrNotFound) {
		t.Fatal("cross account completion read", readErr)
	}
	if winners != 1 {
		t.Fatal("multiple external completion permissions", winners)
	}
	if _, _, err := r.ClaimMultipartCompletion(ctx, uuid.NewString(), u.ID, resource, now); err == nil {
		t.Fatal("cross owner claim")
	}
	if _, err := r.ResolveMultipartCompletion(ctx, u.OwnerID, u.ID, resource, uuid.NewString(), "assembled", "version-one", now); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong operation", err)
	}
	if _, err := r.ResolveMultipartCompletion(ctx, u.OwnerID, u.ID, resource, first.OperationID, "retryable", "", now); err != nil {
		t.Fatal(err)
	}
	second, claimed, err := r.ClaimMultipartCompletion(ctx, u.OwnerID, u.ID, resource, now)
	if err != nil || !claimed || second.OperationID == first.OperationID {
		t.Fatal("known preflight failure not retryable", err)
	}
	if _, err := r.ResolveMultipartCompletion(ctx, u.OwnerID, u.ID, resource, first.OperationID, "assembled", "version-old", now); !errors.Is(err, ErrConflict) {
		t.Fatal("stale worker changed retry", err)
	}
	unknown, err := r.ResolveMultipartCompletion(ctx, u.OwnerID, u.ID, resource, second.OperationID, "unknown", "", now)
	if err != nil {
		t.Fatal(err)
	}
	replay, claimed, err := NewMySQLUploads(db).ClaimMultipartCompletion(ctx, u.OwnerID, u.ID, resource, now.Add(time.Minute))
	if err != nil || claimed || replay != unknown {
		t.Fatal("unknown outcome repeated completion", err)
	}
	if _, err := r.ResolveMultipartCompletion(ctx, u.OwnerID, u.ID, resource, second.OperationID, "retryable", "", now); !errors.Is(err, ErrConflict) {
		t.Fatal("unknown reset to retryable", err)
	}
	checkMySQLReconciliationQueue(t, ctx, db, u, unknown, now)
	assembled, err := r.ResolveMultipartCompletion(ctx, u.OwnerID, u.ID, resource, second.OperationID, "assembled", "version-one", u.ExpiresAt.Add(time.Minute))
	if err != nil || assembled.ObjectVersion != "version-one" {
		t.Fatal("late confirmed version lost", err)
	}
	replay, claimed, err = r.ClaimMultipartCompletion(ctx, u.OwnerID, u.ID, resource, now)
	if err != nil || claimed || replay != assembled {
		t.Fatal("assembled replay", err)
	}
	if _, err := r.ResolveMultipartCompletion(ctx, u.OwnerID, u.ID, resource, second.OperationID, "assembled", "version-two", now); !errors.Is(err, ErrConflict) {
		t.Fatal("version overwritten", err)
	}
	if _, err := r.ResolveMultipartCompletion(ctx, u.OwnerID, u.ID, resource, second.OperationID, "unknown", "", now); !errors.Is(err, ErrConflict) {
		t.Fatal("assembled downgraded", err)
	}
	if _, _, err := r.ClaimMultipartCompletion(ctx, u.OwnerID, u.ID, resource, u.ExpiresAt); !errors.Is(err, ErrExpired) {
		t.Fatal("expired claim", err)
	}
}
