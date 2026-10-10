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

// Called from the real-MySQL upload transaction fixture before submitting its
// parent, so it shares the same migration/replay and bounded database setup.
func checkMySQLMultipartIntent(t *testing.T, ctx context.Context, db *sql.DB, u Upload, now time.Time) {
	t.Helper()
	resourceID := u.Manifest.Resources[0].ID
	type answer struct {
		intent  MultipartIntent
		created bool
		err     error
	}
	answers := make(chan answer, 20)
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			intent, created, err := NewMySQLUploads(db).BeginMultipart(ctx, u.OwnerID, u.ID, resourceID, now)
			answers <- answer{intent, created, err}
		}()
	}
	group.Wait()
	close(answers)
	var first MultipartIntent
	winners := 0
	for value := range answers {
		if value.err != nil {
			t.Fatal(value.err)
		}
		if first.IntentID == "" {
			first = value.intent
		}
		if value.intent != first {
			t.Fatal("different intents for same resource")
		}
		if value.created {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("provider initialization permissions: %d", winners)
	}
	r := NewMySQLUploads(db)
	if _, _, err := r.BeginMultipart(ctx, u.OwnerID, u.ID, "missing", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown resource accepted", err)
	}
	other := uuid.NewString()
	if err := r.ProvisionAccount(ctx, other, 0); err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(context.Background(), `DELETE FROM album_storage_accounts WHERE owner_id=?`, other)
	if _, _, err := r.BeginMultipart(ctx, other, u.ID, resourceID, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross account intent exposed", err)
	}
	if _, err := r.ResolveMultipart(ctx, other, u.ID, resourceID, first.IntentID, "provider-one", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross account bound session", err)
	}
	if _, err := r.ResolveMultipart(ctx, u.OwnerID, u.ID, resourceID, uuid.NewString(), "provider-one", now); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong intent accepted", err)
	}
	unknown, err := r.ResolveMultipart(ctx, u.OwnerID, u.ID, resourceID, first.IntentID, "", now)
	if err != nil || unknown.State != "unknown" {
		t.Fatal("lost response not preserved", err)
	}
	replay, created, err := NewMySQLUploads(db).BeginMultipart(ctx, u.OwnerID, u.ID, resourceID, now.Add(time.Minute))
	if err != nil || created || replay != unknown {
		t.Fatal("unknown response reinitialized", err)
	}
	ready, err := r.ResolveMultipart(ctx, u.OwnerID, u.ID, resourceID, first.IntentID, "provider-one", u.ExpiresAt.Add(time.Minute))
	if err != nil || ready.State != "ready" || ready.ProviderUploadID != "provider-one" {
		t.Fatal("late response lost", err)
	}
	for _, provider := range []string{"", "provider-two"} {
		if _, err := r.ResolveMultipart(ctx, u.OwnerID, u.ID, resourceID, first.IntentID, provider, now); !errors.Is(err, ErrConflict) {
			t.Fatal("ready binding changed", err)
		}
	}
	again, err := r.ResolveMultipart(ctx, u.OwnerID, u.ID, resourceID, first.IntentID, "provider-one", now)
	if err != nil || again != ready {
		t.Fatal("binding replay failed", err)
	}
	if _, _, err := r.BeginMultipart(ctx, u.OwnerID, u.ID, resourceID, u.ExpiresAt); !errors.Is(err, ErrExpired) {
		t.Fatal("expired grant intent accepted", err)
	}
}
