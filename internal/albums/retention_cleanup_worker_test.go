package albums

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

func cleanupSubscriptionService(t *testing.T, r *MySQLUploads, owner string, clock *time.Time, status func(context.Context) (VerifiedAlbumSubscription, error)) *AlbumSubscriptionService {
	t.Helper()
	signing := newAlbumReceiptFixture(t, *clock)
	client := reminderStatusFunc(func(ctx context.Context, _ string) ([]appstore.SubscriptionStatus, error) {
		v, err := status(ctx)
		if err != nil {
			return nil, err
		}
		current := signing
		current.now = *clock
		return []appstore.SubscriptionStatus{reminderSignedStatus(t, current, owner, v)}, nil
	})
	return reminderVerificationService(t, r, signing, client, func() time.Time { return *clock })
}

func cleanupWorkerFixture(t *testing.T) (*retentionCleanupWorker, Upload, *time.Time, *VerifiedAlbumSubscription, *purgeStorageFixture) {
	t.Helper()
	r, u, clock, v, _ := retentionPurgeFixture(t)
	s := &purgeStorageFixture{t: t, r: r, asset: u.Manifest.AssetID, versions: map[string][]DerivedObjectVersion{}}
	service := cleanupSubscriptionService(t, r, u.OwnerID, clock, func(context.Context) (VerifiedAlbumSubscription, error) { return v, nil })
	w := &retentionCleanupWorker{repository: r, subscriptions: service, storage: s, batchSize: 10, now: func() time.Time { return *clock }}
	return w, u, clock, &v, s
}

func TestMySQLRetentionCleanupWorkerMetadataOnlyAndReplay(t *testing.T) {
	r, owner, now, v := reminderRepository(t)
	v.ExpiresAt = v.ExpiresAt.Truncate(time.Millisecond)
	now = albumRetentionDeadline(v.ExpiresAt)
	r.billingNow = func() time.Time { return now }
	if _, err := r.db.Exec(`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,SHA2(?,256))`, owner, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`UPDATE album_storage_accounts SET used_bytes=0 WHERE owner_id=?`, owner); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := r.db.Exec(`INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at) VALUES(?,?,1,UUID(),'retained group',FALSE,?)`, owner, uuid.NewString(), v.ExpiresAt); err != nil {
			t.Fatal(err)
		}
	}
	applyReminderSubscription(t, r, owner, v, now)
	calls := 0
	service := cleanupSubscriptionService(t, r, owner, &now, func(context.Context) (VerifiedAlbumSubscription, error) { calls++; return v, nil })
	w := &retentionCleanupWorker{repository: r, subscriptions: service, storage: &purgeStorageFixture{t: t}, batchSize: 1, now: func() time.Time { return now }}
	b, err := w.runOnce(context.Background())
	if err != nil || b.Collections != 2 || b.Deferred != 0 || b.Advanced != 0 || calls != 1 {
		t.Fatal("metadata-only expiry", b, calls, err)
	}
	var retained int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM album_collections WHERE owner_id=? AND deleted=TRUE AND name=''`, owner).Scan(&retained); err != nil || retained != 2 {
		t.Fatal("missing deletion markers", retained, err)
	}
	now = now.Add(time.Minute)
	w.repository = NewMySQLUploads(r.db).WithSubscriptionEnvironment("Sandbox", w.now)
	b, err = w.runOnce(context.Background())
	if err != nil || b != (retentionCleanupBatch{}) || calls != 1 {
		t.Fatal("completed groups scheduled again after restart", b, calls, err)
	}
}

func TestMySQLRetentionCleanupWorkerEmptyKeysKeepProgressAcrossFreshProofs(t *testing.T) {
	w, u, clock, _, storage := cleanupWorkerFixture(t)
	ctx := context.Background()
	settled := 0
	for range 8 {
		b, err := w.runOnce(ctx)
		if err != nil || b.Deferred != 0 {
			t.Fatal(b, err)
		}
		settled += b.Settled
		*clock = clock.Add(31 * time.Second)
		if settled > 0 {
			break
		}
		w.repository = NewMySQLUploads(w.repository.db).WithSubscriptionEnvironment("Sandbox", w.now)
	}
	if settled != 1 || len(storage.deletes) != 0 {
		t.Fatal("empty keys never settled across refreshed proofs", settled, storage.deletes)
	}
	used, reserved := photoJournalBalance(t, w.repository.db, u.OwnerID)
	if used != 0 || reserved != 0 {
		t.Fatal("settlement did not release capacity", used, reserved)
	}
	var receipts int
	if err := w.repository.db.QueryRow(`SELECT COUNT(*) FROM album_asset_deletion_ledger WHERE owner_id=? AND asset_id=? AND reason='retention'`, u.OwnerID, u.Manifest.AssetID).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal("missing atomic deletion fence", receipts, err)
	}
}

func TestMySQLRetentionCleanupWorkerLostDeleteAndRenewalResumeGrantedPlan(t *testing.T) {
	w, u, clock, v, storage := cleanupWorkerFixture(t)
	ctx := context.Background()
	proof := verifyPurgeFixture(t, w.repository, u.OwnerID, *v, clock)
	p, err := w.repository.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.repository.db.Exec(`DELETE FROM album_object_purges WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	storage.versions[p.Targets[0].Key] = []DerivedObjectVersion{{"immutable-version", false, 4}}
	storage.loseOnce = true
	b, err := w.runOnce(ctx)
	if err != nil || b.Deferred != 1 || len(storage.deletes) != 1 {
		t.Fatal("lost delete not retained", b, err)
	}
	renewPurgeFixture(t, w.repository, u.OwnerID, *v, *clock)
	*clock = clock.Add(11 * time.Minute)
	settled := 0
	for range 6 {
		b, err = w.runOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		settled += b.Settled
		*clock = clock.Add(time.Minute)
		if settled > 0 {
			break
		}
	}
	if settled != 1 || len(storage.deletes) != 2 || storage.deletes[0] != storage.deletes[1] {
		t.Fatal("did not retry exact granted delete after renewal", settled, storage.deletes)
	}
	a, err := w.repository.CloudAccount(ctx, u.OwnerID, *clock)
	if err != nil || a.State != "active" || a.UsedBytes != 0 {
		t.Fatal("renewal lost or old media resurrected", a, err)
	}
}

func TestMySQLRetentionCleanupWorkerSafetyFences(t *testing.T) {
	for _, fault := range []string{"before-deadline", "renewal", "provider", "open-write", "bounded-sandbox", "development", "quarantine"} {
		t.Run(fault, func(t *testing.T) {
			w, u, clock, v, storage := cleanupWorkerFixture(t)
			seedCollections(t, w.repository.db, u.OwnerID)
			if fault == "before-deadline" {
				*clock = albumRetentionDeadline(v.ExpiresAt).Add(-time.Microsecond)
			}
			if fault == "bounded-sandbox" {
				w.repository.productionSandboxQuota = 1_000_000_000
			}
			if fault == "development" {
				w.repository.developmentCloud = &DevelopmentCloudConfig{}
			}
			if fault == "quarantine" {
				if _, err := w.repository.db.Exec(`UPDATE album_recovery_state SET quarantined=TRUE,quarantined_at=? WHERE singleton_id=1`, *clock); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "open-write" {
				if _, err := w.repository.db.Exec(`UPDATE album_uploads SET derived_budget_closed=FALSE WHERE owner_id=?`, u.OwnerID); err != nil {
					t.Fatal(err)
				}
			}
			w.subscriptions = cleanupSubscriptionService(t, w.repository, u.OwnerID, clock, func(context.Context) (VerifiedAlbumSubscription, error) {
				if fault == "provider" {
					return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
				}
				if fault == "renewal" {
					renewed := *v
					renewed.Active, renewed.TransactionID, renewed.ExpiresAt = true, "worker-renewal", clock.AddDate(1, 0, 0)
					return renewed, nil
				}
				return *v, nil
			})
			b, err := w.runOnce(context.Background())
			if b.Advanced != 0 || b.Settled != 0 || len(storage.deletes) != 0 || len(storage.aborts) != 0 {
				t.Fatal("unsafe media mutation", b, err)
			}
			if err == nil && fault != "before-deadline" && b.Deferred == 0 {
				t.Fatal("unsafe account not deferred", b)
			}
			var groups int
			if e := w.repository.db.QueryRow(`SELECT COUNT(*) FROM album_collections WHERE owner_id=? AND deleted=FALSE`, u.OwnerID).Scan(&groups); e != nil || fault != "open-write" && groups != 1 {
				// Independent metadata retirement is allowed after a valid expiry proof.
				t.Fatal("unverified metadata erased", groups, e)
			}
		})
	}
}

func TestMySQLRetentionCleanupLeaseRecoveryAndStaleFinish(t *testing.T) {
	w, u, clock, _, _ := cleanupWorkerFixture(t)
	r := w.repository
	var winners atomic.Int32
	var winning retentionCleanupClaim
	var guard sync.Mutex
	var group sync.WaitGroup
	for range 6 {
		group.Go(func() {
			c, err := r.claimRetentionCleanup(context.Background(), u.OwnerID, w.now)
			if err == nil {
				winners.Add(1)
				guard.Lock()
				winning = c
				guard.Unlock()
			} else if !errors.Is(err, ErrLease) {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if winners.Load() != 1 {
		t.Fatal("concurrent owner claims", winners.Load())
	}
	*clock = winning.until
	next, err := r.claimRetentionCleanup(context.Background(), u.OwnerID, w.now)
	if err != nil {
		t.Fatal("crashed claim not recovered", err)
	}
	if err := r.finishRetentionCleanup(context.Background(), winning, "", false, *clock); !errors.Is(err, ErrLease) {
		t.Fatal("old finish replaced new claim", err)
	}
	if err := r.finishRetentionCleanup(context.Background(), next, u.Manifest.AssetID, true, *clock); err != nil {
		t.Fatal(err)
	}
	if _, err := r.claimRetentionCleanup(context.Background(), u.OwnerID, w.now); !errors.Is(err, ErrLease) {
		t.Fatal("failed pass not backed off", err)
	}
	*clock = clock.Add(time.Minute)
	next, err = r.claimRetentionCleanup(context.Background(), u.OwnerID, w.now)
	if err != nil || next.cursor != u.Manifest.AssetID {
		t.Fatal("cursor not durable", next.cursor, err)
	}
}

func TestMySQLRetentionCleanupCursorDoesNotStarveLaterAsset(t *testing.T) {
	w, u, clock, _, _ := cleanupWorkerFixture(t)
	w.batchSize = 1
	blocked := "000-blocked-asset"
	if blocked >= u.Manifest.AssetID {
		t.Fatal("fixture requires earlier blocked asset")
	}
	if _, err := w.repository.db.Exec(`INSERT INTO album_asset_lifecycle(owner_id,asset_id,revision,lifecycle_epoch,state) VALUES(?,?,1,1,'active')`, u.OwnerID, blocked); err != nil {
		t.Fatal(err)
	}
	if _, err := w.repository.db.Exec(`INSERT INTO album_asset_versions(owner_id,asset_id,version_id,manifest_digest,manifest_json,sealed_objects,original_bytes,published_at)
 SELECT owner_id,?,UUID(),manifest_digest,manifest_json,sealed_objects,original_bytes,published_at FROM album_asset_versions WHERE owner_id=? AND version_id=?`, blocked, u.OwnerID, u.ID); err != nil {
		t.Fatal(err)
	}
	settled := 0
	for range 10 {
		b, err := w.runOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		settled += b.Settled
		*clock = clock.Add(time.Minute)
		if settled > 0 {
			break
		}
	}
	if settled != 1 {
		t.Fatal("blocked asset starved later valid media")
	}
	var retained int
	if err := w.repository.db.QueryRow(`SELECT COUNT(*) FROM album_asset_versions WHERE owner_id=? AND asset_id=?`, u.OwnerID, blocked).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("unverified blocked asset removed", retained, err)
	}
}

func TestMySQLRetentionCleanupBackgroundDrainCancelsAndLeavesRecoverableClaim(t *testing.T) {
	w, u, clock, _, _ := cleanupWorkerFixture(t)
	entered := make(chan struct{})
	w.subscriptions = cleanupSubscriptionService(t, w.repository, u.OwnerID, clock, func(ctx context.Context) (VerifiedAlbumSubscription, error) {
		close(entered)
		<-ctx.Done()
		return VerifiedAlbumSubscription{}, ctx.Err()
	})
	g := &Gateway{db: w.repository.db}
	g.startRetentionCleanup(context.Background(), w)
	defer g.BeginDrain()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("background worker not started")
	}
	g.BeginDrain()
	finished := make(chan struct{})
	go func() { g.subscriptionWorkers.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("drain failed to cancel worker")
	}
	var claims int
	if err := w.repository.db.QueryRow(`SELECT COUNT(*) FROM album_storage_accounts WHERE owner_id=? AND retention_cleanup_token IS NOT NULL AND retention_cleanup_until>?`, u.OwnerID, w.now()).Scan(&claims); err != nil || claims != 1 {
		t.Fatal("interrupted claim was lost", claims, err)
	}
}

func TestMySQLRetentionCleanupYieldsToAccountDeletion(t *testing.T) {
	w, u, clock, v, storage := cleanupWorkerFixture(t)
	ctx := context.Background()
	proof := verifyPurgeFixture(t, w.repository, u.OwnerID, *v, clock)
	p, err := w.repository.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock)
	if err != nil {
		t.Fatal(err)
	}
	storage.versions[p.Targets[0].Key] = []DerivedObjectVersion{{"before-deletion-fence", false, 4}}
	if err := w.repository.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, storage, w.now); err != nil {
		t.Fatal(err)
	}
	if _, err := w.repository.db.Exec(`UPDATE album_storage_accounts SET deletion_requested_at=? WHERE owner_id=?`, *clock, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.repository.claimRetentionCleanup(ctx, u.OwnerID, w.now); !errors.Is(err, ErrLease) {
		t.Fatal("claim crossed account deletion fence", err)
	}
	if _, err := w.repository.expireRetentionCollections(ctx, proof, 100, w.now); !errors.Is(err, ErrLease) {
		t.Fatal("old proof crossed account deletion fence", err)
	}
	if err := w.repository.runObjectPurgeStep(ctx, u.OwnerID, u.Manifest.AssetID, storage, w.now); !errors.Is(err, ErrLease) || len(storage.deletes) != 1 {
		t.Fatal("granted plan did not yield namespace to account eraser", storage.deletes, err)
	}
	if b, err := w.runOnce(ctx); err != nil || b != (retentionCleanupBatch{}) {
		t.Fatal("deleting account remained scheduled", b, err)
	}
}

func TestMySQLRetentionCleanupFreshProofWaitsForCurrentPlan(t *testing.T) {
	w, u, clock, v, _ := cleanupWorkerFixture(t)
	ctx := context.Background()
	proof := verifyPurgeFixture(t, w.repository, u.OwnerID, *v, clock)
	if _, err := w.repository.prepareRetentionObjectPurge(ctx, proof, u.Manifest.AssetID, *clock); err != nil {
		t.Fatal(err)
	}
	p, err := w.repository.claimObjectPurge(ctx, u.OwnerID, u.Manifest.AssetID, uuid.NewString(), *clock)
	if err != nil {
		t.Fatal(err)
	}
	fresh := verifyPurgeFixture(t, w.repository, u.OwnerID, *v, clock)
	if _, err := w.repository.prepareRetentionObjectPurge(ctx, fresh, u.Manifest.AssetID, *clock); !errors.Is(err, ErrLease) {
		t.Fatal("current inventory lease was replaced", err)
	}
	*clock = proof.until
	fresh = verifyPurgeFixture(t, w.repository, u.OwnerID, *v, clock)
	replaced, err := w.repository.prepareRetentionObjectPurge(ctx, fresh, u.Manifest.AssetID, *clock)
	if err != nil || replaced.LeaseToken != "" || replaced.Index != p.Index {
		t.Fatal("expired authorization did not yield without losing progress", err)
	}
}

func TestMySQLRetentionCleanupCalendarBoundary(t *testing.T) {
	r, owner, now, v := reminderRepository(t)
	applyReminderSubscription(t, r, owner, v, now)
	if _, err := r.db.Exec(`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,SHA2(?,256))`, owner, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at) VALUES(?,?,1,UUID(),'calendar boundary',FALSE,?)`, owner, uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}
	for _, expiry := range []time.Time{
		time.Date(2026, 8, 31, 23, 59, 59, 123000000, time.UTC),
		time.Date(2023, 8, 31, 23, 59, 59, 123000000, time.UTC),
		time.Date(2026, 1, 31, 23, 59, 59, 123000000, time.UTC),
	} {
		changed, err := r.db.Exec(`UPDATE album_cloud_subscriptions SET expires_at=?,active=FALSE WHERE owner_id=?`, expiry, owner)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := changed.RowsAffected(); err != nil || count != 1 {
			t.Fatal("calendar fixture must contain exactly one subscription", count, err)
		}
		deadline := albumRetentionDeadline(expiry)
		for _, offset := range []time.Duration{-time.Microsecond, 0} {
			owners, err := r.retentionCleanupCandidates(context.Background(), deadline.Add(offset), 10, mediaCleanupMode{retention: true})
			want := 1
			if offset < 0 {
				want = 0
			}
			if err != nil || len(owners) != want || want == 1 && owners[0] != owner {
				t.Fatal("SQL candidate deadline differs from calendar policy", expiry, deadline, offset, owners, err)
			}
		}
	}
}
