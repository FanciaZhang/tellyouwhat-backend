package albums

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func derivedVideoIntent(t *testing.T, r *MySQLUploads, u Upload, target VideoJob, now time.Time, provider bool) VideoJob {
	t.Helper()
	ctx := context.Background()
	j, err := r.ClaimVideoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), now)
	if err != nil {
		t.Fatal(err)
	}
	spec := journalVideoSpec(u)
	spec.MaxBytes = 8 << 20
	j, fresh, err := r.BeginVideoSubmission(ctx, j, spec, now)
	if err != nil || !fresh {
		t.Fatal("could not consume video intent", err)
	}
	if provider {
		j, err = r.AttachVideoSubmission(ctx, j, "j"+strings.Repeat("b", 32), now)
		if err != nil {
			t.Fatal(err)
		}
	}
	return j
}
func derivedCleanupFor(t *testing.T, r *MySQLUploads, owner, kind, reference string) DerivedCleanup {
	t.Helper()
	c, err := scanCleanup(r.db.QueryRow(`SELECT `+cleanupColumns+` FROM album_derived_cleanups WHERE owner_id=? AND kind=? AND reference_id=?`, owner, kind, reference))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func closeVideoForCleanup(t *testing.T, r *MySQLUploads, u Upload, j VideoJob, now time.Time) DerivedCleanup {
	t.Helper()
	if err := r.FinishVideoStep(context.Background(), j, true, true, now); err != nil {
		t.Fatal(err)
	}
	f, err := r.FinalizeDerivedBudget(context.Background(), DerivedVersionRef{u.OwnerID, u.ID}, now)
	if err != nil || f.HeldBytes != j.Spec.MaxBytes || f.ReleasedBytes != u.DerivedBudget-j.Spec.MaxBytes {
		t.Fatal("incorrect held closure", f, err)
	}
	return derivedCleanupFor(t, r, u.OwnerID, "video", j.ID)
}

func TestMySQLDerivedFinalizationClosesOnceAcrossConcurrentWorkers(t *testing.T) {
	db := photoJournalDB(t)
	u, target := videoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	j := derivedVideoIntent(t, r, u, target, now, true)
	result := journalVideoResult(j)
	if _, err := r.CompleteVideoJob(ctx, j, result, now); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			f, err := NewMySQLUploads(db).FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now)
			if err != nil || f.RetainedBytes != result.Object.SizeBytes || f.HeldBytes != 0 || f.ReleasedBytes != u.DerivedBudget-result.Object.SizeBytes {
				t.Error("closure not idempotent", f, err)
			}
		}()
	}
	group.Wait()
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes+result.Object.SizeBytes || reserved != 0 {
		t.Fatal("closure charged or refunded twice", used, reserved)
	}
	var closed bool
	if err := db.QueryRow(`SELECT derived_budget_closed FROM album_uploads WHERE owner_id=? AND upload_id=?`, u.OwnerID, u.ID).Scan(&closed); err != nil || !closed {
		t.Fatal("closed upload remains in pending scheduling range", closed, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_derived_cleanups WHERE owner_id=?`, u.OwnerID).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate cleanup intent", count, err)
	}
	reopened := NewMySQLUploads(photoJournalDB(t))
	if _, err := reopened.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now.Add(time.Hour)); err != nil {
		t.Fatal("reopen lost closure", err)
	}
	if _, err := reopened.DerivedFinalization(ctx, uuid.NewString(), u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-owner closure exposed", err)
	}
	j, err := reopened.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if err != nil || j.Result == nil || *j.Result != result {
		t.Fatal("valid receipt lost during closure", err)
	}
}

func TestMySQLDerivedFinalizationPendingAndUnspentBudget(t *testing.T) {
	db := photoJournalDB(t)
	u, candidate := photoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("queued work prematurely closed", err)
	}
	_, reserved := photoJournalBalance(t, db, u.OwnerID)
	if reserved != u.DerivedBudget {
		t.Fatal("pending budget released", reserved)
	}
	if _, err := db.Exec(`UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	f, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now)
	if err != nil || f.ReleasedBytes != u.DerivedBudget || f.HeldBytes != 0 || f.RetainedBytes != 0 {
		t.Fatal("unspent budget not released", f, err)
	}
	if _, fresh, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, photoJournalSpec(u, candidate, PhotoGridPNG), now); !errors.Is(err, ErrDerivedClosed) || fresh {
		t.Fatal("closed budget admitted new PUT", fresh, err)
	}
	provider := &queuedPhotoProvider{objectTestStore: &objectTestStore{}, candidate: candidate}
	service, err := NewUploadService(r, provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GeneratePhotoDerivative(ctx, u.OwnerID, u.ID, PhotoGridPNG); !errors.Is(err, ErrDerivedClosed) || provider.renders.Load() != 0 || provider.puts.Load() != 0 {
		t.Fatal("closed version incurred new rendering cost", err)
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes || reserved != 0 {
		t.Fatal("original accounting changed", used, reserved)
	}
}

func TestMySQLDerivedFinalizationQuotaMismatchRollsBack(t *testing.T) {
	db := photoJournalDB(t)
	u, target := videoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	j := derivedVideoIntent(t, r, u, target, now, false)
	if err := r.FinishVideoStep(ctx, j, true, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE album_storage_accounts SET reserved_bytes=0 WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now); !errors.Is(err, ErrQuota) {
		t.Fatal("corrupt balance accepted", err)
	}
	if _, err := r.DerivedFinalization(ctx, u.OwnerID, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("partial closure committed", err)
	}
	if _, err := db.Exec(`UPDATE album_storage_accounts SET reserved_bytes=? WHERE owner_id=?`, u.DerivedBudget, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	f, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now)
	if err != nil || f.HeldBytes != j.Spec.MaxBytes {
		t.Fatal("retry after rollback failed", err)
	}
}

type oneDerivedCleanupQueue struct {
	*MySQLUploads
	owner, id string
}

func (q oneDerivedCleanupQueue) PendingDerivedCleanups(ctx context.Context, now time.Time, _ int) ([]DerivedCleanup, error) {
	c, err := q.DerivedCleanup(ctx, q.owner, q.id)
	if err != nil {
		return nil, err
	}
	var available time.Time
	if err := q.db.QueryRowContext(ctx, `SELECT available_at FROM album_derived_cleanups WHERE cleanup_id=?`, q.id).Scan(&available); err != nil {
		return nil, err
	}
	if (c.State == "queued" && !available.After(now)) || (c.State == "running" && !c.LeaseUntil.After(now)) {
		return []DerivedCleanup{c}, nil
	}
	return nil, nil
}

type cleanupProviderFixture struct {
	versions              []DerivedObjectVersion
	deletes               []string
	observation           VideoCleanupObservation
	inspectErr, verifyErr error
	receipt               *SealedObject
	invalidPhoto          bool
	loseDeleteOnce        bool
	beforeDelete          func(DerivedCleanupTarget, DerivedObjectVersion)
}

func (p *cleanupProviderFixture) ListDerivedVersions(_ context.Context, _ DerivedCleanupTarget) ([]DerivedObjectVersion, error) {
	return append([]DerivedObjectVersion(nil), p.versions...), nil
}
func (p *cleanupProviderFixture) DeleteDerivedVersion(_ context.Context, target DerivedCleanupTarget, v DerivedObjectVersion) error {
	if p.beforeDelete != nil {
		p.beforeDelete(target, v)
	}
	p.deletes = append(p.deletes, v.VersionID)
	for i, x := range p.versions {
		if x == v {
			p.versions = append(p.versions[:i], p.versions[i+1:]...)
			break
		}
	}
	if p.loseDeleteOnce {
		p.loseDeleteOnce = false
		return ErrCOSOperation
	}
	return nil
}
func (p *cleanupProviderFixture) AssessPhotoCleanup(context.Context, PhotoWriteRecord, []DerivedObjectVersion) (*SealedObject, bool, error) {
	return p.receipt, p.invalidPhoto, nil
}
func (p *cleanupProviderFixture) InspectVideoCleanup(context.Context, VideoJob) (VideoCleanupObservation, error) {
	return p.observation, p.inspectErr
}
func (p *cleanupProviderFixture) VerifyVideoResult(_ context.Context, j VideoJob) (VideoResult, error) {
	if p.verifyErr != nil {
		return VideoResult{}, p.verifyErr
	}
	return journalVideoResult(j), nil
}
func cleanupTestWorker(t *testing.T, r *MySQLUploads, c DerivedCleanup, p *cleanupProviderFixture, now *time.Time) *DerivedCleanupWorker {
	t.Helper()
	w, err := NewDerivedCleanupWorker(oneDerivedCleanupQueue{r, c.OwnerID, c.ID}, p, 1, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestMySQLDerivedCleanupFailedVideoLostDeleteReopensAndRefundsOnce(t *testing.T) {
	db := photoJournalDB(t)
	u, target := videoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	j := derivedVideoIntent(t, r, u, target, now, true)
	c := closeVideoForCleanup(t, r, u, j, now)
	p := &cleanupProviderFixture{versions: []DerivedObjectVersion{{"partial+version&?", false, 100}, {"marker", true, 0}, {"historical", false, 17}}, observation: VideoCleanupObservation{j.ProviderJobID, true, false}, loseDeleteOnce: true}
	p.beforeDelete = func(target DerivedCleanupTarget, _ DerivedObjectVersion) {
		key, err := target.key()
		if err != nil || !strings.HasPrefix(key, "albums/video-previews/"+u.OwnerID+"/"+j.ID+"/") {
			t.Fatal("wrong delete namespace", key)
		}
		current, err := r.DerivedCleanup(ctx, c.OwnerID, c.ID)
		job, e := r.VideoJob(ctx, j.OwnerID, j.VersionID, j.Preset)
		if err != nil || e != nil || current.Plan == nil || job.RetirementStartedAt.IsZero() {
			t.Fatal("delete before durable retirement fence", err, e)
		}
	}
	w := cleanupTestWorker(t, r, c, p, &now)
	b, err := w.RunOnce(ctx)
	if err != nil || b.Failed != 1 || len(p.deletes) != 1 {
		t.Fatal("lost delete was not retained", b, err)
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes || reserved != j.Spec.MaxBytes {
		t.Fatal("unknown delete released quota", used, reserved)
	}
	reopened := NewMySQLUploads(photoJournalDB(t))
	now = now.Add(16 * time.Minute)
	w = cleanupTestWorker(t, reopened, c, p, &now)
	b, err = w.RunOnce(ctx)
	if err != nil || b.Retired != 1 || len(p.deletes) != 3 || len(p.versions) != 0 {
		t.Fatal("restart failed to finish exact-version cleanup", b, err, len(p.deletes))
	}
	used, reserved = photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes || reserved != 0 {
		t.Fatal("incorrect retirement transfer", used, reserved)
	}
	f, err := reopened.DerivedFinalization(ctx, u.OwnerID, u.ID)
	if err != nil || f.HeldBytes != 0 || f.ReleasedBytes != u.DerivedBudget {
		t.Fatal("ledger not settled", f, err)
	}
	if b, err = w.RunOnce(ctx); err != nil || b != (DerivedCleanupBatch{}) || len(p.deletes) != 3 {
		t.Fatal("completed cleanup reran", b, err)
	}
	j, err = reopened.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if err != nil || j.RetiredAt.IsZero() || j.Result != nil {
		t.Fatal("retirement marker missing", err)
	}
	if _, err := reopened.CompleteVideoJob(ctx, j, journalVideoResult(j), now); !errors.Is(err, ErrDerivedRetired) {
		t.Fatal("late receipt resurrected retired output", err)
	}
}

func TestMySQLDerivedCleanupUnknownOrRunningVideoRetainsEscrow(t *testing.T) {
	for _, scenario := range []string{"unknown", "running", "network", "success-unverified"} {
		t.Run(scenario, func(t *testing.T) {
			db := photoJournalDB(t)
			u, target := videoJournalUpload(t, db)
			r := NewMySQLUploads(db)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)
			j := derivedVideoIntent(t, r, u, target, now, true)
			c := closeVideoForCleanup(t, r, u, j, now)
			p := &cleanupProviderFixture{versions: []DerivedObjectVersion{{"output", false, 100}}}
			switch scenario {
			case "running":
				p.observation = VideoCleanupObservation{j.ProviderJobID, false, false}
			case "network":
				p.inspectErr = ErrVideoProcessing
			case "success-unverified":
				p.observation = VideoCleanupObservation{j.ProviderJobID, true, true}
				p.verifyErr = ErrVideoProcessing
			}
			w := cleanupTestWorker(t, r, c, p, &now)
			b, err := w.RunOnce(ctx)
			if err != nil || b.Waiting+b.Failed != 1 || len(p.deletes) != 0 {
				t.Fatal("uncertain task was deleted", b, err)
			}
			_, reserved := photoJournalBalance(t, db, u.OwnerID)
			if reserved != j.Spec.MaxBytes {
				t.Fatal("uncertain escrow refunded", reserved)
			}
			stored, err := r.DerivedCleanup(ctx, c.OwnerID, c.ID)
			job, e := r.VideoJob(ctx, j.OwnerID, j.VersionID, j.Preset)
			if err != nil || e != nil || stored.Plan != nil || !job.RetirementStartedAt.IsZero() {
				t.Fatal("uncertain task marked for retirement", err, e)
			}
		})
	}
}

func TestMySQLDerivedCleanupRecoversDiscoveredVideoAndProtectsReceipt(t *testing.T) {
	db := photoJournalDB(t)
	u, target := videoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	j := derivedVideoIntent(t, r, u, target, now, false)
	c := closeVideoForCleanup(t, r, u, j, now)
	p := &cleanupProviderFixture{observation: VideoCleanupObservation{"j" + strings.Repeat("b", 32), true, true}, versions: []DerivedObjectVersion{{"output-version", false, 4096}, {"extra-version", false, 4096}, {"marker", true, 0}}}
	w := cleanupTestWorker(t, r, c, p, &now)
	b, err := w.RunOnce(ctx)
	if err != nil || b.Recovered != 1 || len(p.deletes) != 0 {
		t.Fatal("late valid video not recovered", b, err)
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes+4096 || reserved != 0 {
		t.Fatal("recovery did not settle bounded escrow", used, reserved)
	}
	stored, err := r.DerivedCleanup(ctx, c.OwnerID, c.ID)
	if err != nil || stored.KeepVersion != "output-version" || stored.ReserveBytes != 0 || stored.LeaseToken != "" || stored.Attempts != 0 {
		t.Fatal("old cleanup lease not fenced", stored, err)
	}
	b, err = w.RunOnce(ctx)
	if err != nil || b.Pruned != 1 || len(p.deletes) != 2 || len(p.versions) != 1 || p.versions[0].VersionID != "output-version" {
		t.Fatal("recovered receipt not protected", b, err, p.deletes)
	}
	j, err = r.VideoJob(ctx, j.OwnerID, j.VersionID, j.Preset)
	if err != nil || j.Result == nil || j.State != "completed" || j.ProviderJobID != p.observation.ProviderJobID {
		t.Fatal("recovered provider job lost", err)
	}
}

func TestMySQLDerivedCleanupPhotoRecoveryFencesConcurrentRetirement(t *testing.T) {
	db := photoJournalDB(t)
	u, candidate := photoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	spec := photoJournalSpec(u, candidate, PhotoGridPNG)
	p, _, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	f, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now)
	if err != nil || f.HeldBytes != spec.SizeBytes {
		t.Fatal("photo escrow missing", err)
	}
	c := derivedCleanupFor(t, r, u.OwnerID, "photo", spec.AttemptID)
	claimed, err := r.ClaimDerivedCleanup(ctx, c.OwnerID, c.ID, uuid.NewString(), now)
	if err != nil {
		t.Fatal(err)
	}
	receipt := photoJournalReceipt(spec)
	if _, err := r.CompletePhotoWrite(ctx, p, receipt, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.StoreDerivedCleanupPlan(ctx, claimed, DerivedCleanupPlan{Terminal: true, Versions: []DerivedObjectVersion{{receipt.VersionID, false, receipt.SizeBytes}}}, now); !errors.Is(err, ErrLease) {
		t.Fatal("stale cleanup could retire recovered photo", err)
	}
	if _, err := r.CompletePhotoWrite(ctx, p, receipt, now); err != nil {
		t.Fatal("photo replay failed", err)
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes+spec.SizeBytes || reserved != 0 {
		t.Fatal("photo late receipt charged twice", used, reserved)
	}
	stored, err := r.DerivedCleanup(ctx, c.OwnerID, c.ID)
	if err != nil || stored.KeepVersion != receipt.VersionID || stored.ReserveBytes != 0 {
		t.Fatal("receipt protection absent", err)
	}
}

func TestMySQLDerivedCleanupClaimsAndExpiryFenceConcurrentWorkers(t *testing.T) {
	db := photoJournalDB(t)
	u, target := videoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	j := derivedVideoIntent(t, r, u, target, now, true)
	c := closeVideoForCleanup(t, r, u, j, now)
	var group sync.WaitGroup
	won := make(chan DerivedCleanup, 20)
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			claimed, err := r.ClaimDerivedCleanup(ctx, c.OwnerID, c.ID, uuid.NewString(), now)
			if err == nil {
				won <- claimed
			} else if !errors.Is(err, ErrLease) {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	close(won)
	if len(won) != 1 {
		t.Fatal("more than one active cleanup lease", len(won))
	}
	old := <-won
	now = now.Add(11 * time.Minute)
	fresh, err := r.ClaimDerivedCleanup(ctx, c.OwnerID, c.ID, uuid.NewString(), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.StoreDerivedCleanupPlan(ctx, old, DerivedCleanupPlan{Terminal: true}, now); !errors.Is(err, ErrLease) {
		t.Fatal("expired worker stored retirement plan", err)
	}
	if err := r.FinishDerivedRetirement(ctx, old, now); !errors.Is(err, ErrLease) {
		t.Fatal("expired worker refunded quota", err)
	}
	if _, err := r.StoreDerivedCleanupPlan(ctx, fresh, DerivedCleanupPlan{Terminal: true}, now); err != nil {
		t.Fatal("fresh lease refused terminal plan", err)
	}
	if err := r.FinishDerivedRetirement(ctx, fresh, now); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishDerivedRetirement(ctx, fresh, now); !errors.Is(err, ErrLease) {
		t.Fatal("completed lease still allowed retirement", err)
	}
	_, reserved := photoJournalBalance(t, db, u.OwnerID)
	if reserved != 0 {
		t.Fatal("quota refund failed", reserved)
	}
}

func TestMySQLDerivedCleanupInvalidPhotoCannotPublishAfterRetirement(t *testing.T) {
	db := photoJournalDB(t)
	u, candidate := photoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	spec := photoJournalSpec(u, candidate, PhotoGridPNG)
	p, _, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now); err != nil {
		t.Fatal(err)
	}
	c := derivedCleanupFor(t, r, u.OwnerID, "photo", spec.AttemptID)
	storage := &cleanupProviderFixture{versions: []DerivedObjectVersion{{"invalid-committed", false, spec.SizeBytes + 1}, {"marker", true, 0}}, invalidPhoto: true, loseDeleteOnce: true}
	w := cleanupTestWorker(t, r, c, storage, &now)
	b, err := w.RunOnce(ctx)
	if err != nil || b.Failed != 1 {
		t.Fatal("lost photo delete not retained", b, err)
	}
	if _, err := r.CompletePhotoWrite(ctx, p, photoJournalReceipt(spec), now); !errors.Is(err, ErrDerivedRetired) {
		t.Fatal("late photo completion overrode retirement fence", err)
	}
	now = now.Add(16 * time.Minute)
	r = NewMySQLUploads(photoJournalDB(t))
	w = cleanupTestWorker(t, r, c, storage, &now)
	b, err = w.RunOnce(ctx)
	if err != nil || b.Retired != 1 || len(storage.versions) != 0 {
		t.Fatal("photo retirement did not recover", b, err)
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes || reserved != 0 {
		t.Fatal("invalid photo charged or refunded incorrectly", used, reserved)
	}
	stored, err := r.PhotoWriteByAttempt(ctx, u.OwnerID, spec.AttemptID)
	if err != nil || stored.RetiredAt.IsZero() || stored.ObjectVersion != "" {
		t.Fatal("invalid photo retirement not persisted", err)
	}
}

func TestMySQLDerivedCleanupProtectsMissingReceiptAndUnexpectedNewVersions(t *testing.T) {
	for _, scenario := range []string{"missing-receipt", "changed-receipt-size", "unplanned-version"} {
		t.Run(scenario, func(t *testing.T) {
			db := photoJournalDB(t)
			u, target := videoJournalUpload(t, db)
			r := NewMySQLUploads(db)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)
			j := derivedVideoIntent(t, r, u, target, now, true)
			if scenario != "unplanned-version" {
				if _, err := r.CompleteVideoJob(ctx, j, journalVideoResult(j), now); err != nil {
					t.Fatal(err)
				}
				if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := r.FinishVideoStep(ctx, j, true, true, now); err != nil {
					t.Fatal(err)
				}
				if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now); err != nil {
					t.Fatal(err)
				}
			}
			c := derivedCleanupFor(t, r, u.OwnerID, "video", j.ID)
			storage := &cleanupProviderFixture{versions: []DerivedObjectVersion{{"unreferenced", false, 100}}, observation: VideoCleanupObservation{j.ProviderJobID, true, scenario != "unplanned-version"}}
			if scenario == "changed-receipt-size" {
				storage.versions = append(storage.versions, DerivedObjectVersion{"output-version", false, 4097})
			}
			if scenario == "unplanned-version" {
				claimed, err := r.ClaimDerivedCleanup(ctx, c.OwnerID, c.ID, uuid.NewString(), now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.StoreDerivedCleanupPlan(ctx, claimed, DerivedCleanupPlan{Terminal: true, Versions: []DerivedObjectVersion{{"old-planned", false, 100}}}, now); err != nil {
					t.Fatal(err)
				}
				if err := r.FinishDerivedCleanupStep(ctx, claimed, false, now); err != nil {
					t.Fatal(err)
				}
				now = now.Add(16 * time.Minute)
			}
			w := cleanupTestWorker(t, r, c, storage, &now)
			b, err := w.RunOnce(ctx)
			if err != nil || b.Failed != 1 || len(storage.deletes) != 0 {
				t.Fatal("unsafe snapshot deleted", b, err)
			}
			current, err := r.DerivedCleanup(ctx, c.OwnerID, c.ID)
			if err != nil || current.State != "attention" {
				t.Fatal("unsafe snapshot did not stop", err)
			}
			_, reserved := photoJournalBalance(t, db, u.OwnerID)
			expected := int64(0)
			if scenario == "unplanned-version" {
				expected = j.Spec.MaxBytes
			}
			if reserved != expected {
				t.Fatal("unsafe snapshot changed escrow", reserved)
			}
		})
	}
}
