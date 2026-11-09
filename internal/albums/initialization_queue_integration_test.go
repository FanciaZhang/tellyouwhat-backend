package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func initializationQueueFixture(t *testing.T) (*MySQLUploads, Upload, MultipartIntent, time.Time) {
	t.Helper()
	ctx := context.Background()
	db, _ := gatewayTestDatabase(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := NewMySQLUploads(db)
	_, _, objects, owner, manifest := uploadFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := r.ProvisionAccount(ctx, owner, 1<<20); err != nil {
		t.Fatal(err)
	}
	s, err := NewUploadService(r, objects, UploadPolicy{1 << 20, 1 << 20, time.Hour, time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Create(ctx, owner, uuid.NewString(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	intent, fresh, err := r.BeginMultipart(ctx, owner, u.ID, "photo", now)
	if err != nil || !fresh {
		t.Fatal("initial intent", fresh, err)
	}
	return r, u, intent, now
}

func TestMySQLInitializationReconciliationScheduled(t *testing.T) {
	r, u, intent, now := initializationQueueFixture(t)
	jobs, err := r.PendingReconciliations(context.Background(), now.Add(3*time.Minute), 100)
	if err != nil || len(jobs) != 1 || jobs[0].OwnerID != u.OwnerID || jobs[0].OperationID != intent.IntentID {
		t.Fatal("lost initialization is not scheduled", jobs, err)
	}
	if jobs[0].Kind != "initialization" {
		t.Fatal("wrong recovery kind", jobs)
	}
	checkReconciliationQueue(t, context.Background(), r, jobs[0], now)
}

func TestMySQLInitializationWorkerRecoversWithoutNewWrites(t *testing.T) {
	for _, scenario := range []string{"initiating", "unknown", "expired", "missing", "cancel", "database"} {
		t.Run(scenario, func(t *testing.T) {
			r, u, intent, now := initializationQueueFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "unknown" {
				if _, err := r.ResolveMultipart(ctx, u.OwnerID, u.ID, "photo", intent.IntentID, "", now); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(3 * time.Minute)
			if scenario == "expired" {
				now = u.ExpiresAt.Add(time.Minute)
			}
			calls := 0
			provider := &multipartProviderStub{find: func(_ context.Context, key string) (string, error) {
				calls++
				if key != stagingKey(u, "photo") {
					t.Fatal("wrong recovery key")
				}
				if scenario == "missing" {
					return "", ErrMultipartPending
				}
				if scenario == "cancel" {
					cancel()
				}
				if scenario == "database" {
					if _, err := r.db.Exec(`ALTER TABLE album_multipart_intents ADD CONSTRAINT test_initialization_resolution_failure CHECK(state<>'ready')`); err != nil {
						t.Fatal(err)
					}
				}
				return "recovered-provider-upload", nil
			}}
			service, err := NewMultipartService(NewMySQLUploads(r.db), provider, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			worker, err := NewReconciliationWorker(NewMySQLUploads(r.db), service, 100)
			if err != nil {
				t.Fatal(err)
			}
			batch, err := worker.RunOnce(ctx)
			if calls != 1 || scenario == "cancel" && !errors.Is(err, context.Canceled) || scenario != "cancel" && err != nil {
				t.Fatal("recovery execution", batch, calls, err)
			}
			stored, err := r.GetMultipart(context.Background(), u.OwnerID, u.ID, "photo")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "missing" || scenario == "database" {
				if stored != intent || batch.Failed != 1 || batch.Initialized != 0 {
					t.Fatal("unknown outcome changed", stored, batch)
				}
			} else if stored.State != "ready" || stored.ProviderUploadID != "recovered-provider-upload" || scenario != "cancel" && batch.Initialized != 1 {
				t.Fatal("lost recovered session", stored, batch)
			}
			var used, reserved int64
			if err := r.db.QueryRow(`SELECT used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, u.OwnerID).Scan(&used, &reserved); err != nil || used != 0 || reserved != u.OriginalBytes+u.DerivedBudget {
				t.Fatal("recovery changed quota", used, reserved, err)
			}
			parent, err := r.Get(context.Background(), u.OwnerID, u.ID)
			if err != nil || parent.State != Uploading || !parent.ExpiresAt.Equal(u.ExpiresAt) {
				t.Fatal("recovery published or renewed parent", parent, err)
			}
			if scenario == "expired" {
				if _, err := service.Part(context.Background(), u.OwnerID, u.ID, "photo", 1); !errors.Is(err, ErrExpired) {
					t.Fatal("expired parent gained write permission", err)
				}
			}
			jobs, err := r.PendingReconciliations(context.Background(), now, 100)
			if err != nil || len(jobs) != 0 {
				t.Fatal("resolved or backed-off job requeued immediately", jobs, err)
			}
		})
	}
}

func TestMySQLInitializationMigrationAndMixedQueue(t *testing.T) {
	r, u, intent, now := initializationQueueFixture(t)
	ctx := context.Background()
	// Recreate the pre-migration table with a real unresolved intent.
	if _, err := r.db.Exec(`ALTER TABLE album_multipart_intents DROP INDEX album_initialization_reconcile_queue, DROP COLUMN reconcile_attempts, DROP COLUMN reconcile_token, DROP COLUMN reconcile_until, DROP COLUMN reconcile_after`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`DELETE FROM schema_migrations WHERE name='0039_album_initialization_reconciliation.sql'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PendingReconciliations(ctx, now, 1); err == nil {
		t.Fatal("old schema accepted")
	}
	if err := migrations.Run(ctx, r.db); err != nil {
		t.Fatal(err)
	}
	stored, err := r.GetMultipart(ctx, u.OwnerID, u.ID, "photo")
	if err != nil || stored != intent {
		t.Fatal("migration changed original intent", stored, err)
	}
	// A separate upload has an unresolved completion; both kinds share one limit.
	other := u
	other.ID, other.RequestID, other.Manifest.AssetID = uuid.NewString(), uuid.NewString(), "another-upload"
	other.Digest, err = other.Manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	other, err = r.Reserve(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := r.BeginMultipart(ctx, other.OwnerID, other.ID, "photo", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveMultipart(ctx, other.OwnerID, other.ID, "photo", second.IntentID, "known-session", now); err != nil {
		t.Fatal(err)
	}
	completion, fresh, err := r.ClaimMultipartCompletion(ctx, other.OwnerID, other.ID, "photo", now)
	if err != nil || !fresh {
		t.Fatal("completion intent", fresh, err)
	}
	jobs, err := r.PendingReconciliations(ctx, now.Add(3*time.Minute), 100)
	if err != nil || len(jobs) != 2 {
		t.Fatal("mixed inventory", jobs, err)
	}
	found := map[string]string{}
	for _, job := range jobs {
		found[job.Kind] = job.OperationID
	}
	if found["initialization"] != intent.IntentID || found["completion"] != completion.OperationID {
		t.Fatal("mixed operation identities", found)
	}
	limited, err := r.PendingReconciliations(ctx, now.Add(3*time.Minute), 1)
	if err != nil || len(limited) != 1 || limited[0] != jobs[0] {
		t.Fatal("union ignored deterministic batch limit", limited, err)
	}
	bad := jobs[0]
	bad.Kind = "album_accounts"
	if _, err := r.ClaimReconciliation(ctx, bad, now.Add(3*time.Minute)); !errors.Is(err, ErrInvalidManifest) {
		t.Fatal("unknown queue kind accepted", err)
	}
}
