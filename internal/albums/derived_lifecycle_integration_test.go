package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Commit the same lifecycle boundary under the account lock; media fixtures use
// internal capacity provisioning, independently of Apple subscription fixtures.
func setDerivedAssetTrash(t *testing.T, db *sql.DB, u Upload, trash bool, now time.Time) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = lockAccount(context.Background(), tx, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	state := "active"
	var start, end any
	if trash {
		state, start, end = "trashed", now, now.Add(30*24*time.Hour)
	}
	result, err := tx.Exec(`UPDATE album_asset_lifecycle SET state=?,trashed_at=?,trash_expires_at=?,revision=revision+1,lifecycle_epoch=lifecycle_epoch+1 WHERE owner_id=? AND asset_id=?`, state, start, end, u.OwnerID, u.Manifest.AssetID)
	if err != nil {
		t.Fatal(err)
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		t.Fatal("missing fixture lifecycle", n, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLTrashFencesFreshPhotoWriteButKeepsConsumedEvidence(t *testing.T) {
	db := photoJournalDB(t)
	u, candidate := photoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	spec := photoJournalSpec(u, candidate, PhotoGridPNG)
	setDerivedAssetTrash(t, db, u, true, now)
	if _, fresh, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, now); !errors.Is(err, ErrNotFound) || fresh {
		t.Fatal("trashed photo granted new PUT", fresh, err)
	}
	if _, err := r.PhotoWrite(ctx, u.OwnerID, u.ID, spec.Preset); !errors.Is(err, ErrNotFound) {
		t.Fatal("blocked write left intent", err)
	}
	for _, j := range ownPhotoJobs(t, r, u.OwnerID) {
		if _, err := r.ClaimPhotoJob(ctx, u.OwnerID, j.ID, uuid.NewString(), now); !errors.Is(err, ErrLease) {
			t.Fatal("trashed job claimed", err)
		}
	}
	jobs, err := r.PendingPhotoJobs(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.OwnerID == u.OwnerID {
			t.Fatal("trash fills photo queue")
		}
	}
	setDerivedAssetTrash(t, db, u, false, now)
	record, fresh, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, now)
	if err != nil || !fresh {
		t.Fatal("restore did not resume", fresh, err)
	}
	setDerivedAssetTrash(t, db, u, true, now)
	replay, fresh, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, now)
	if err != nil || fresh || replay.Spec != record.Spec {
		t.Fatal("consumed intent lost or duplicated", fresh, err)
	}
	if _, err := r.CompletePhotoWrite(ctx, record, photoJournalReceipt(spec), now); err != nil {
		t.Fatal("late receipt cannot be accounted", err)
	}
}

func TestMySQLTrashFencesFreshVideoSubmissionAndPreservesLostReply(t *testing.T) {
	db := photoJournalDB(t)
	u, target := videoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	claimed, err := r.ClaimVideoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), now)
	if err != nil {
		t.Fatal(err)
	}
	setDerivedAssetTrash(t, db, u, true, now)
	if _, fresh, err := r.BeginVideoSubmission(ctx, claimed, journalVideoSpec(u), now); !errors.Is(err, ErrNotFound) || fresh {
		t.Fatal("trashed video granted paid POST", fresh, err)
	}
	jobs, err := r.PendingVideoJobs(ctx, now.Add(11*time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.OwnerID == u.OwnerID {
			t.Fatal("trash fills video queue")
		}
	}
	if _, err := r.ClaimVideoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), now.Add(11*time.Minute)); !errors.Is(err, ErrLease) {
		t.Fatal("trashed video reclaimed", err)
	}
	setDerivedAssetTrash(t, db, u, false, now)
	submitted, fresh, err := r.BeginVideoSubmission(ctx, claimed, journalVideoSpec(u), now)
	if err != nil || !fresh {
		t.Fatal("restore did not resume", fresh, err)
	}
	setDerivedAssetTrash(t, db, u, true, now)
	if _, fresh, err := r.BeginVideoSubmission(ctx, submitted, journalVideoSpec(u), now); err != nil || fresh {
		t.Fatal("consumed POST lost or duplicated", fresh, err)
	}
	submitted, err = r.AttachVideoSubmission(ctx, submitted, "j"+strings.Repeat("b", 32), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CompleteVideoJob(ctx, submitted, journalVideoResult(submitted), now); err != nil {
		t.Fatal("late video receipt rejected", err)
	}
}

func TestMySQLTrashFencesLivePairReservationGrantAndEachSeal(t *testing.T) {
	db := photoJournalDB(t)
	u, descriptor := livePairJournalFixture(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := livePairTestService(t, r, &now, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected storage request"); return nil, nil })
	pair, err := service.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	setDerivedAssetTrash(t, db, u, true, now)
	if _, err := service.LivePairGrants(ctx, u.OwnerID, pair.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("trash issued staging PUT", err)
	}
	if _, err := r.ReserveLivePair(ctx, pair, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("trash reservation replay accepted", err)
	}
	if _, err := r.SubmitLivePair(ctx, u.OwnerID, pair.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("trash queued pair", err)
	}
	setDerivedAssetTrash(t, db, u, false, now)
	if _, err := r.SubmitLivePair(ctx, u.OwnerID, pair.ID, now); err != nil {
		t.Fatal(err)
	}
	setDerivedAssetTrash(t, db, u, true, now)
	if _, err := r.ClaimLivePair(ctx, u.OwnerID, pair.ID, uuid.NewString(), now, time.Minute); !errors.Is(err, ErrLease) {
		t.Fatal("trashed pair claimed", err)
	}
	jobs, err := r.PendingLivePairs(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.OwnerID == u.OwnerID {
			t.Fatal("trash fills pair queue")
		}
	}
	setDerivedAssetTrash(t, db, u, false, now)
	claimed, err := r.ClaimLivePair(ctx, u.OwnerID, pair.ID, uuid.NewString(), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	photo, fresh, err := r.BeginLivePairSeal(ctx, claimed, "photo", now)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	setDerivedAssetTrash(t, db, u, true, now)
	if _, fresh, err := r.BeginLivePairSeal(ctx, photo, "motion", now); !errors.Is(err, ErrNotFound) || fresh {
		t.Fatal("trash consumed second COPY", fresh, err)
	}
	if _, fresh, err := r.BeginLivePairSeal(ctx, photo, "photo", now); err != nil || fresh {
		t.Fatal("consumed COPY lost or duplicated", fresh, err)
	}
	key, _ := photo.key("photo", false)
	if _, err := r.CompleteLivePairPart(ctx, photo, SealedObject{"photo", key, "late-pair-version", descriptor.Photo.SizeBytes, descriptor.Photo.SHA256}, now); err != nil {
		t.Fatal("late pair receipt rejected", err)
	}
}

func TestMySQLLivePairGrantLocksTrashAndLatestSubscription(t *testing.T) {
	db := photoJournalDB(t)
	u, descriptor := livePairJournalFixture(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clock := time.Now().UTC().Truncate(time.Microsecond)
	r := NewMySQLUploads(db)
	service := livePairTestService(t, r, &clock, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected provider HTTP"); return nil, nil })
	pair, err := service.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	r.WithSubscriptionEnvironment("Sandbox", func() time.Time { return clock })
	decision := VerifiedAlbumSubscription{uuid.NewString(), uuid.NewString(), Cloud50GBAnnualProduct, "Sandbox", clock.Add(time.Minute), clock, true}
	account, err := r.ApplySubscription(ctx, u.OwnerID, 0, decision, clock)
	if err != nil {
		t.Fatal(err)
	}
	// Lock-held signing must work even when the pool has exactly one connection.
	db.SetMaxOpenConns(1)
	calls := 0
	err = r.WithLivePairUploadGrant(ctx, pair, clock, clock.Add(10*time.Minute), func(until time.Time) error {
		calls++
		if !until.Equal(decision.ExpiresAt) {
			t.Error("grant outlived latest subscription", until)
		}
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatal("single-connection grant", calls, err)
	}
	changed := pair
	changed.ManifestDigest = strings.Repeat("0", 64)
	if err = r.WithLivePairUploadGrant(ctx, changed, clock, clock.Add(time.Minute), func(time.Time) error { t.Error("changed snapshot signed"); return nil }); !errors.Is(err, ErrConflict) {
		t.Fatal("stale snapshot", err)
	}
	start := clock
	err = r.WithLivePairUploadGrant(ctx, pair, start, start.Add(10*time.Minute), func(time.Time) error { clock = decision.ExpiresAt; return nil })
	if !errors.Is(err, ErrExpired) {
		t.Fatal("signing crossed cutoff", err)
	}
	clock = start
	// A newly committed expiry overrides the earlier caller snapshot.
	decision.Active = false
	decision.SignedAt = clock.Add(time.Second)
	if _, err = r.ApplySubscription(ctx, u.OwnerID, account.Revision, decision, clock); err != nil {
		t.Fatal(err)
	}
	if err = r.WithLivePairUploadGrant(ctx, pair, clock, clock.Add(time.Minute), func(time.Time) error { t.Error("expired subscription signed"); return nil }); !errors.Is(err, ErrSubscriptionRequired) {
		t.Fatal("latest expiry ignored", err)
	}
	r.billingEnvironment = ""
	r.billingNow = nil
	db.SetMaxOpenConns(5)
	entered, release := make(chan struct{}), make(chan struct{})
	signed := make(chan error, 1)
	go func() {
		signed <- r.WithLivePairUploadGrant(ctx, pair, clock, clock.Add(time.Minute), func(time.Time) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	deleting := make(chan error, 1)
	go func() {
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			deleting <- e
			return
		}
		defer tx.Rollback()
		if _, e = lockAccount(ctx, tx, u.OwnerID); e != nil {
			deleting <- e
			return
		}
		_, e = tx.ExecContext(ctx, `UPDATE album_asset_lifecycle SET state='trashed',trashed_at=?,trash_expires_at=? WHERE owner_id=? AND asset_id=?`, clock, clock.Add(30*24*time.Hour), u.OwnerID, u.Manifest.AssetID)
		if e == nil {
			e = tx.Commit()
		}
		deleting <- e
	}()
	select {
	case err := <-deleting:
		close(release)
		t.Fatal("trash crossed in-flight signing lock", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err := <-signed; err != nil {
		t.Fatal(err)
	}
	if err := <-deleting; err != nil {
		t.Fatal(err)
	}
	if err = r.WithLivePairUploadGrant(ctx, pair, clock, clock.Add(time.Minute), func(time.Time) error { t.Error("post-trash grant"); return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

type lifecyclePhotoProvider struct {
	*queuedPhotoProvider
	afterRead func()
}

func (p *lifecyclePhotoProvider) RenderPhotoCandidate(ctx context.Context, source SealedObject, preset PhotoPreset) (PhotoCandidate, error) {
	p.afterRead()
	return p.queuedPhotoProvider.RenderPhotoCandidate(ctx, source, preset)
}

type lifecycleVideoProvider struct {
	*journalVideoProvider
	afterRead func()
}

func (p *lifecycleVideoProvider) InspectVideoSource(ctx context.Context, source SealedObject) (VideoProfile, error) {
	p.afterRead()
	return p.journalVideoProvider.InspectVideoSource(ctx, source)
}

func TestMySQLTrashDuringProcessingPausesAndRestorationResumesWorkers(t *testing.T) {
	t.Run("photo", func(t *testing.T) {
		db := photoJournalDB(t)
		u, c := photoJournalUpload(t, db)
		r := NewMySQLUploads(db)
		ctx := context.Background()
		now := time.Now().UTC()
		_, _, objects, _, _ := uploadFixture(t)
		provider := &lifecyclePhotoProvider{&queuedPhotoProvider{objectTestStore: objects, candidate: c}, func() { setDerivedAssetTrash(t, db, u, true, now) }}
		service, err := NewUploadService(r, provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		var target PhotoJob
		for _, j := range ownPhotoJobs(t, r, u.OwnerID) {
			if j.Preset == PhotoGridPNG {
				target = j
			}
		}
		worker, err := NewPhotoWorker(onePhotoQueue{r, target.ID}, service, 1)
		if err != nil {
			t.Fatal(err)
		}
		if batch, err := worker.RunOnce(ctx); err != nil || batch.Failed != 1 || provider.puts.Load() != 0 {
			t.Fatal("trash admitted write", batch, err)
		}
		for _, j := range ownPhotoJobs(t, r, u.OwnerID) {
			if j.ID == target.ID && j.State != "queued" {
				t.Fatal("temporary trash became terminal", j.State)
			}
		}
		if batch, err := worker.RunOnce(ctx); err != nil || batch.Failed+batch.Stored != 0 {
			t.Fatal("trash retried", batch, err)
		}
		provider.afterRead = func() {}
		setDerivedAssetTrash(t, db, u, false, now)
		now = now.Add(time.Minute)
		if batch, err := worker.RunOnce(ctx); err != nil || batch.Stored != 1 || provider.puts.Load() != 1 {
			t.Fatal("restoration did not resume once", batch, err)
		}
	})
	t.Run("video", func(t *testing.T) {
		db := photoJournalDB(t)
		u, target := videoJournalUpload(t, db)
		r := NewMySQLUploads(db)
		ctx := context.Background()
		now := time.Now().UTC()
		_, _, objects, _, _ := uploadFixture(t)
		provider := &lifecycleVideoProvider{&journalVideoProvider{objectTestStore: objects}, func() { setDerivedAssetTrash(t, db, u, true, now) }}
		service, err := NewUploadService(r, provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		worker, err := NewVideoWorker(oneVideoQueue{r, target.ID}, service, "p"+strings.Repeat("d", 32), 1)
		if err != nil {
			t.Fatal(err)
		}
		if batch, err := worker.RunOnce(ctx); err != nil || batch.Failed != 1 || provider.submissions.Load() != 0 {
			t.Fatal("trash admitted transcode", batch, err)
		}
		job, err := r.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
		if err != nil || job.State != "queued" {
			t.Fatal("temporary trash became terminal", job.State, err)
		}
		if batch, err := worker.RunOnce(ctx); err != nil || batch.Failed+batch.Stored != 0 {
			t.Fatal("trash retried", batch, err)
		}
		provider.afterRead = func() {}
		setDerivedAssetTrash(t, db, u, false, now)
		now = now.Add(time.Minute)
		if batch, err := worker.RunOnce(ctx); err != nil || batch.Waiting != 1 || provider.submissions.Load() != 1 {
			t.Fatal("restoration did not resume once", batch, err)
		}
	})
}
