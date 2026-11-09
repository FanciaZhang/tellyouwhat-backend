package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func trashCleanupFixture(t *testing.T) (*retentionCleanupWorker, Upload, *time.Time, *purgeStorageFixture) {
	t.Helper()
	db, u, now := purgeFixture(t)
	if _, err := db.Exec(`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,SHA2(?,256))`, u.OwnerID, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	r := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return now })
	// Accelerated Sandbox subscription expiry never authorizes retention erasure;
	// a user's actual 30-day trash deadline is an independent deletion choice.
	r.productionSandboxQuota = 1_000_000_000
	s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID, versions: map[string][]DerivedObjectVersion{}}
	w := &retentionCleanupWorker{repository: r, storage: s, batchSize: 10, now: func() time.Time { return now }, trashEnabled: true}
	return w, u, &now, s
}

func runCleanupToSettlement(t *testing.T, w *retentionCleanupWorker, clock *time.Time) int {
	t.Helper()
	for range 12 {
		b, err := w.runOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if b.Settled > 0 {
			return b.Settled
		}
		*clock = clock.Add(time.Minute)
	}
	return 0
}

func TestMySQLTrashCleanupRunsWithoutSubscriptionsAtExactDeadline(t *testing.T) {
	w, u, clock, storage := trashCleanupFixture(t)
	deadline := *clock
	*clock = clock.Add(-time.Microsecond)
	if b, err := w.runOnce(context.Background()); err != nil || b != (retentionCleanupBatch{}) {
		t.Fatal("trash admitted before its deadline", b, err)
	}
	*clock = deadline
	if n := runCleanupToSettlement(t, w, clock); n != 1 {
		t.Fatal("trash did not settle without Apple verification", n)
	}
	used, reserved := photoJournalBalance(t, w.repository.db, u.OwnerID)
	if used != 0 || reserved != 0 || len(storage.deletes) != 0 {
		t.Fatal("empty-key settlement or capacity invalid", used, reserved, storage.deletes)
	}
	var count int
	if err := w.repository.db.QueryRow(`SELECT COUNT(*) FROM album_asset_deletion_ledger WHERE owner_id=? AND asset_id=? AND reason='trash'`, u.OwnerID, u.Manifest.AssetID).Scan(&count); err != nil || count != 1 {
		t.Fatal("missing trash deletion fence", count, err)
	}
	if b, err := w.runOnce(context.Background()); err != nil || b != (retentionCleanupBatch{}) {
		t.Fatal("completed trash repeated", b, err)
	}
}

func TestMySQLTrashCleanupLostReplyResumesExactPlanAfterRestart(t *testing.T) {
	w, u, clock, storage := trashCleanupFixture(t)
	ctx := context.Background()
	p, err := w.repository.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, *clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.repository.db.Exec(`DELETE FROM album_object_purges WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	storage.versions[p.Targets[0].Key] = []DerivedObjectVersion{{"trash-version", false, 4}}
	storage.loseOnce = true
	if b, err := w.runOnce(ctx); err != nil || b.Deferred != 1 || len(storage.deletes) != 1 {
		t.Fatal("lost response was not preserved", b, err)
	}
	used, _ := photoJournalBalance(t, w.repository.db, u.OwnerID)
	if used != u.OriginalBytes {
		t.Fatal("unconfirmed removal refunded quota", used)
	}
	*clock = clock.Add(11 * time.Minute)
	w.repository = NewMySQLUploads(w.repository.db).WithSubscriptionEnvironment("Sandbox", w.now)
	w.repository.productionSandboxQuota = 1_000_000_000
	storage.r = w.repository
	if n := runCleanupToSettlement(t, w, clock); n != 1 || len(storage.deletes) != 2 || storage.deletes[0] != storage.deletes[1] {
		t.Fatal("exact immutable delete not resumed", n, storage.deletes)
	}
}

func TestMySQLTrashCleanupContinuesWhenRetentionVerificationFails(t *testing.T) {
	w, u, clock, _, storage := cleanupWorkerFixture(t)
	setDerivedAssetTrash(t, w.repository.db, u, true, clock.Add(-30*24*time.Hour))
	w.trashEnabled = true
	w.subscriptions = cleanupSubscriptionService(t, w.repository, u.OwnerID, clock, func(context.Context) (VerifiedAlbumSubscription, error) {
		return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
	})
	if n := runCleanupToSettlement(t, w, clock); n != 1 || len(storage.deletes) != 0 {
		t.Fatal("Apple outage blocked independent trash choice", n, storage.deletes)
	}
	var reason string
	if err := w.repository.db.QueryRow(`SELECT reason FROM album_asset_deletion_ledger WHERE owner_id=? AND asset_id=?`, u.OwnerID, u.Manifest.AssetID).Scan(&reason); err != nil || reason != "trash" {
		t.Fatal("trash borrowed subscription authority", reason, err)
	}
}

func TestMySQLTrashCleanupDoesNotExtendFinalRetentionCutoff(t *testing.T) {
	for _, trashEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "retention-only", true: "both"}[trashEnabled], func(t *testing.T) {
			w, u, clock, v, _ := cleanupWorkerFixture(t)
			w.trashEnabled = trashEnabled
			v.ExpiresAt = clock.AddDate(0, -6, 0).Add(5 * time.Second)
			v.TransactionID = "near-final-cutoff"
			v.SignedAt = clock.Add(time.Millisecond)
			applyReminderSubscription(t, w.repository, u.OwnerID, *v, *clock)
			cutoff := albumRetentionDeadline(v.ExpiresAt)
			*clock = cutoff.Add(-time.Microsecond)
			setDerivedAssetTrash(t, w.repository.db, u, true, clock.Add(-24*time.Hour))
			state, err := w.repository.CloudAsset(context.Background(), u.OwnerID, u.Manifest.AssetID, *clock)
			if err != nil || state.TrashExpiresAt == nil || !state.TrashExpiresAt.After(cutoff) || state.RestoreUntil == nil || !state.RestoreUntil.Equal(cutoff) {
				t.Fatal("public restore window exceeded final cutoff", state, err)
			}
			if b, err := w.runOnce(context.Background()); err != nil || b != (retentionCleanupBatch{}) {
				t.Fatal("future trash was erased before final cutoff", b, err)
			}
			*clock = cutoff
			if n := runCleanupToSettlement(t, w, clock); n != 1 {
				t.Fatal("future trash deadline extended final retention", n)
			}
			var reason string
			if err := w.repository.db.QueryRow(`SELECT reason FROM album_asset_deletion_ledger WHERE owner_id=? AND asset_id=?`, u.OwnerID, u.Manifest.AssetID).Scan(&reason); err != nil || reason != "retention" {
				t.Fatal("wrong cutoff authority", reason, err)
			}
		})
	}
}

func TestMySQLTrashCleanupExistingPlanConvergesUnderRetentionOnly(t *testing.T) {
	w, u, clock, _, _ := cleanupWorkerFixture(t)
	setDerivedAssetTrash(t, w.repository.db, u, true, clock.Add(-30*24*time.Hour))
	p, err := w.repository.prepareExpiredTrashPurge(context.Background(), u.OwnerID, u.Manifest.AssetID, *clock)
	if err != nil {
		t.Fatal(err)
	}
	if n := runCleanupToSettlement(t, w, clock); n != 1 {
		t.Fatal("independent trash plan blocked final expiry", n)
	}
	completed, err := w.repository.objectPurge(context.Background(), u.OwnerID, u.Manifest.AssetID)
	if err != nil || completed.State != "completed" || completed.Reason != p.Reason {
		t.Fatal("retention rewrote the existing trash authority", completed, err)
	}
}

func TestMySQLTrashCleanupAdoptsOnlyUnstartedRetentionAndFencesOldReader(t *testing.T) {
	for _, kind := range []string{"empty-progress", "leased-inventory"} {
		t.Run(kind, func(t *testing.T) {
			w, u, clock, v, storage := cleanupWorkerFixture(t)
			setDerivedAssetTrash(t, w.repository.db, u, true, clock.Add(-30*24*time.Hour))
			ctx := context.Background()
			proof := verifyPurgeFixture(t, w.repository, u.OwnerID, *v, clock)
			if _, err := w.repository.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock); err != nil {
				t.Fatal(err)
			}
			if kind == "empty-progress" {
				if err := w.repository.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, storage, w.now); err != nil {
					t.Fatal(err)
				}
			}
			old, err := w.repository.claimObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, uuid.NewString(), *clock)
			if err != nil {
				t.Fatal(err)
			}
			old, err = w.repository.storeObjectPurgePlan(ctx, old, purgePlan{Versions: []DerivedObjectVersion{{"unstarted-plan-version", false, 4}}}, *clock)
			if err != nil {
				t.Fatal(err)
			}
			adopted, err := w.repository.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, *clock)
			if err != nil || adopted.Reason != "trash" || adopted.Environment != "" || adopted.LeaseToken != "" || adopted.Index != old.Index {
				t.Fatal("user trash choice did not retain progress", adopted, err)
			}
			if _, err := w.repository.authorizeObjectPurgeAction(ctx, old, w.now); !errors.Is(err, ErrLease) {
				t.Fatal("late retention reader survived authority change", err)
			}
			w.subscriptions, w.trashEnabled = nil, true
			if n := runCleanupToSettlement(t, w, clock); n != 1 {
				t.Fatal("adopted trash did not settle", n)
			}
		})
	}
}

func TestMySQLTrashCleanupCannotAdoptGrantedRetention(t *testing.T) {
	w, u, clock, v, storage := cleanupWorkerFixture(t)
	ctx := context.Background()
	proof := verifyPurgeFixture(t, w.repository, u.OwnerID, *v, clock)
	p, err := w.repository.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock)
	if err != nil {
		t.Fatal(err)
	}
	storage.versions[p.Targets[0].Key] = []DerivedObjectVersion{{"granted-retention-version", false, 4}}
	if err := w.repository.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, storage, w.now); err != nil {
		t.Fatal(err)
	}
	if _, err := w.repository.prepareExpiredTrashPurge(ctx, u.OwnerID, u.Manifest.AssetID, *clock); !errors.Is(err, ErrConflict) {
		t.Fatal("granted retention was changed to a new authority", err)
	}
	after, err := w.repository.objectPurge(ctx, u.OwnerID, u.Manifest.AssetID)
	if err != nil || after.Reason != "retention" || after.StartedAt.IsZero() {
		t.Fatal("granted deletion evidence changed", after, err)
	}
}

func TestMySQLTrashCleanupPreservesUnreadyAndProtectedMedia(t *testing.T) {
	for _, fault := range []string{"open-write", "deleting", "quarantine", "restored", "development", "disabled", "environment"} {
		t.Run(fault, func(t *testing.T) {
			w, u, clock, storage := trashCleanupFixture(t)
			switch fault {
			case "open-write":
				if _, err := w.repository.db.Exec(`UPDATE album_uploads SET derived_budget_closed=FALSE WHERE owner_id=?`, u.OwnerID); err != nil {
					t.Fatal(err)
				}
			case "deleting":
				if _, err := w.repository.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=? WHERE owner_id=?`, *clock, u.OwnerID); err != nil {
					t.Fatal(err)
				}
				if _, err := w.repository.prepareExpiredTrashPurge(context.Background(), u.OwnerID, u.Manifest.AssetID, *clock); !errors.Is(err, ErrLease) {
					t.Fatal("stale trash preparation crossed account deletion fence", err)
				}
			case "quarantine":
				if _, err := w.repository.db.Exec(`UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=? WHERE singleton_id=1`, *clock); err != nil {
					t.Fatal(err)
				}
			case "restored":
				setDerivedAssetTrash(t, w.repository.db, u, false, *clock)
			case "development":
				w.repository.developmentCloud = &DevelopmentCloudConfig{}
			case "disabled":
				w.trashEnabled = false
			case "environment":
				w.repository.billingEnvironment = "Both"
			}
			b, err := w.runOnce(context.Background())
			if b.Advanced != 0 || b.Settled != 0 || len(storage.deletes) != 0 || len(storage.aborts) != 0 {
				t.Fatal("protected media mutated", b, err)
			}
			if fault == "open-write" && (err != nil || b.Deferred != 1) {
				t.Fatal("open write did not remain pending", b, err)
			}
			used, reserved := photoJournalBalance(t, w.repository.db, u.OwnerID)
			if used != u.OriginalBytes || reserved != 0 {
				t.Fatal("protected capacity changed", used, reserved)
			}
			var versions int
			if err := w.repository.db.QueryRow(`SELECT COUNT(*) FROM album_asset_versions WHERE owner_id=?`, u.OwnerID).Scan(&versions); err != nil || versions != 1 {
				t.Fatal("protected original metadata lost", versions, err)
			}
		})
	}
}

type blockingTrashInventory struct {
	*purgeStorageFixture
	entered chan struct{}
}

func (s *blockingTrashInventory) listPurgeVersions(ctx context.Context, _ string, _ purgeTarget) ([]DerivedObjectVersion, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestMySQLTrashCleanupDrainPreservesClaimsAndRecovers(t *testing.T) {
	w, u, clock, storage := trashCleanupFixture(t)
	blocked := &blockingTrashInventory{storage, make(chan struct{})}
	w.storage = blocked
	g := &Gateway{db: w.repository.db}
	g.startRetentionCleanup(context.Background(), w)
	defer g.BeginDrain()
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("trash inventory did not start")
	}
	g.BeginDrain()
	finished := make(chan struct{})
	go func() { g.subscriptionWorkers.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("trash inventory did not stop during drain")
	}
	var ownerClaims int
	if err := w.repository.db.QueryRow(`SELECT COUNT(*) FROM album_storage_accounts WHERE owner_id=? AND retention_cleanup_token IS NOT NULL`, u.OwnerID).Scan(&ownerClaims); err != nil || ownerClaims != 1 {
		t.Fatal("interrupted owner claim lost", ownerClaims, err)
	}
	p, err := w.repository.objectPurge(context.Background(), u.OwnerID, u.Manifest.AssetID)
	if err != nil || p.State != "running" || p.Plan != nil || len(storage.deletes) != 0 {
		t.Fatal("cancelled inventory gained mutation authority", p, err)
	}
	w.storage = storage
	*clock = clock.Add(11 * time.Minute)
	if n := runCleanupToSettlement(t, w, clock); n != 1 {
		t.Fatal("interrupted trash plan did not recover", n)
	}
}
