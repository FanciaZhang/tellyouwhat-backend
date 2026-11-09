package albums

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func videoJournalUpload(t *testing.T, db *sql.DB) (Upload, VideoJob) {
	t.Helper()
	_, _, _, owner, manifest := uploadFixture(t)
	manifest.Kind = Video
	manifest.Resources = []Resource{{ID: "movie", Role: OriginalVideo, SizeBytes: 12345678, SHA256: strings.Repeat("c", 64), Descriptor: &ResourceDescriptor{2, "source.MOV", "com.apple.quicktime-movie"}}}
	manifest.Metadata = &AssetMetadata{PixelWidth: 1920, PixelHeight: 1080, DurationMilliseconds: 1000}
	digest, err := manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	u := Upload{ID: uuid.NewString(), OwnerID: owner, RequestID: uuid.NewString(), Manifest: manifest, Digest: digest, State: Uploading,
		OriginalBytes: manifest.Resources[0].SizeBytes, DerivedBudget: 64 << 20, ExpiresAt: now.Add(time.Hour)}
	r := NewMySQLUploads(db)
	ctx := context.Background()
	if err := r.ProvisionAccount(ctx, owner, 1<<30); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM album_derived_cleanups WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_derived_finalizations WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_video_jobs WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_asset_versions WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_uploads WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_storage_accounts WHERE owner_id=?`, owner)
	})
	if _, err := r.Reserve(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, owner, u.ID, now); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	if _, err := r.Claim(ctx, owner, u.ID, token, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	object := SealedObject{"movie", sealedKey(u, "movie"), "source-version", u.OriginalBytes, manifest.Resources[0].SHA256}
	u, err = r.Complete(ctx, owner, u.ID, token, []SealedObject{object}, now)
	if err != nil {
		t.Fatal(err)
	}
	j, err := r.VideoJob(ctx, owner, u.ID, VideoDaily1080)
	if err != nil {
		t.Fatal(err)
	}
	return u, j
}

func journalVideoSpec(u Upload) VideoTranscodeSpec {
	return VideoTranscodeSpec{Source: u.Objects[0], Profile: VideoProfile{1920, 1080, 1000, 60, 2}, MaxBytes: 64 << 20, QueueID: "p" + strings.Repeat("d", 32)}
}

func journalVideoResult(j VideoJob) VideoResult {
	key, _ := j.outputKey()
	return VideoResult{SealedObject{string(j.Preset), key, "output-version", 4096, strings.Repeat("e", 64)}, j.Spec.Profile}
}

func TestMySQLVideoPublicationClaimAndSubmissionAreOnceAndLeaseFenced(t *testing.T) {
	db := photoJournalDB(t)
	u, target := videoJournalUpload(t, db)
	r, ctx := NewMySQLUploads(db), context.Background()
	if err := r.CheckVideoSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for range 3 {
		if _, err := r.Complete(ctx, u.OwnerID, u.ID, "", nil, now); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_video_jobs WHERE owner_id=?`, u.OwnerID).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate publication", err, count)
	}
	var group sync.WaitGroup
	won := make(chan VideoJob, 20)
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			j, err := r.ClaimVideoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), now)
			if err == nil {
				won <- j
			} else if !errors.Is(err, ErrLease) {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	close(won)
	if len(won) != 1 {
		t.Fatal("duplicate active processing lease", len(won))
	}
	claimed := <-won
	spec := journalVideoSpec(u)
	var submissions atomic.Int32
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, fresh, err := r.BeginVideoSubmission(ctx, claimed, spec, now)
			if err != nil {
				t.Error(err)
			}
			if fresh {
				submissions.Add(1)
			}
		}()
	}
	group.Wait()
	if submissions.Load() != 1 {
		t.Fatal("more than one paid POST permission", submissions.Load())
	}
	after := claimed.LeaseUntil.Add(time.Second)
	second, err := r.ClaimVideoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), after)
	if err != nil || second.Spec == nil || second.SubmittedAt.IsZero() || *second.Spec != spec {
		t.Fatal("durable intent not resumed", err)
	}
	if _, err := r.AttachVideoSubmission(ctx, claimed, "j"+strings.Repeat("b", 32), after); !errors.Is(err, ErrInvalidManifest) && !errors.Is(err, ErrLease) {
		t.Fatal("stale worker attached a job", err)
	}
	second, err = r.AttachVideoSubmission(ctx, second, "j"+strings.Repeat("b", 32), after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AttachVideoSubmission(ctx, second, second.ProviderJobID, after); err != nil {
		t.Fatal("idempotent attachment failed", err)
	}
	if _, err := r.AttachVideoSubmission(ctx, second, "j"+strings.Repeat("f", 32), after); !errors.Is(err, ErrLease) {
		t.Fatal("provider job rebound", err)
	}
	if err := r.FinishVideoStep(ctx, claimed, false, false, after); !errors.Is(err, ErrLease) {
		t.Fatal("stale worker cleared new lease", err)
	}
	if _, err := r.ClaimVideoJob(ctx, uuid.NewString(), target.ID, uuid.NewString(), after); !errors.Is(err, ErrLease) {
		t.Fatal("cross-owner claim", err)
	}
}

type journalVideoProvider struct {
	*objectTestStore
	lost, ready              bool
	inspections, submissions atomic.Int32
	discoveries, polls       atomic.Int32
	verifications            atomic.Int32
}

func (p *journalVideoProvider) InspectVideoSource(context.Context, SealedObject) (VideoProfile, error) {
	p.inspections.Add(1)
	return VideoProfile{1920, 1080, 1000, 60, 2}, nil
}
func (p *journalVideoProvider) SubmitVideoOnce(context.Context, VideoJob) (string, error) {
	p.submissions.Add(1)
	if p.lost {
		return "", ErrVideoUncertain
	}
	return "j" + strings.Repeat("b", 32), nil
}
func (p *journalVideoProvider) FindVideoSubmission(context.Context, VideoJob) (string, error) {
	p.discoveries.Add(1)
	return "j" + strings.Repeat("b", 32), nil
}
func (p *journalVideoProvider) PollVideoSubmission(context.Context, VideoJob) (bool, error) {
	p.polls.Add(1)
	return p.ready, nil
}
func (p *journalVideoProvider) VerifyVideoResult(_ context.Context, j VideoJob) (VideoResult, error) {
	p.verifications.Add(1)
	return journalVideoResult(j), nil
}
func (p *journalVideoProvider) AuthorizeVideoPreviewRead(context.Context, VideoJob, time.Time) (string, error) {
	return "https://storage.example/fixed-video", nil
}

type oneVideoQueue struct {
	*MySQLUploads
	id string
}

func (q oneVideoQueue) PendingVideoJobs(ctx context.Context, now time.Time, limit int) ([]VideoJob, error) {
	jobs, err := q.MySQLUploads.PendingVideoJobs(ctx, now, 100)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.ID == q.id {
			return []VideoJob{j}, nil
		}
	}
	return nil, nil
}

func videoJournalService(t *testing.T, r *MySQLUploads, now *time.Time, provider *journalVideoProvider) *UploadService {
	t.Helper()
	_, _, objects, _, _ := uploadFixture(t)
	provider.objectTestStore = objects
	s, err := NewUploadService(r, provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMySQLVideoLostPOSTResumesAfterDatabaseReopenAndChargesOnce(t *testing.T) {
	db := photoJournalDB(t)
	u, target := videoJournalUpload(t, db)
	r, ctx := NewMySQLUploads(db), context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	provider := &journalVideoProvider{lost: true, ready: true}
	service := videoJournalService(t, r, &now, provider)
	worker, _ := NewVideoWorker(oneVideoQueue{r, target.ID}, service, journalVideoSpec(u).QueueID, 1)
	batch, err := worker.RunOnce(ctx)
	if err != nil || batch.Failed != 1 || provider.submissions.Load() != 1 {
		t.Fatal("lost response not recorded", batch, err)
	}
	j, _ := r.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if j.Spec == nil || j.SubmittedAt.IsZero() || j.ProviderJobID != "" || j.Result != nil {
		t.Fatal("unknown outcome not durable")
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes || reserved != u.DerivedBudget {
		t.Fatal("unknown response fabricated a receipt")
	}
	reopened := photoJournalDB(t)
	r = NewMySQLUploads(reopened)
	now = now.Add(2 * time.Minute)
	service = videoJournalService(t, r, &now, provider)
	worker, _ = NewVideoWorker(oneVideoQueue{r, target.ID}, service, journalVideoSpec(u).QueueID, 1)
	batch, err = worker.RunOnce(ctx)
	if err != nil || batch.Stored != 1 || provider.inspections.Load() != 1 || provider.submissions.Load() != 1 || provider.discoveries.Load() != 1 || provider.verifications.Load() != 1 {
		t.Fatal("restart repeated paid processing", batch, err)
	}
	j, err = r.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if err != nil || j.State != "completed" || j.Result == nil {
		t.Fatal("result not committed", err)
	}
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := r.CompleteVideoJob(ctx, j, *j.Result, now); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	used, reserved = photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes+4096 || reserved != u.DerivedBudget-4096 {
		t.Fatal("completion double-charged", used, reserved)
	}
	for range 3 {
		grant, err := service.VideoPreviewReadGrant(ctx, u.OwnerID, u.ID, VideoDaily1080)
		if err != nil || grant.ReplacementEligible || grant.Profile.FrameRate != 60 || grant.ObjectVersionID != "output-version" {
			t.Fatal("wrong video grant", err)
		}
	}
	if _, err := service.VideoPreviewReadGrant(ctx, uuid.NewString(), u.ID, VideoDaily1080); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-account result exposed", err)
	}
	newUsed, newReserved := photoJournalBalance(t, db, u.OwnerID)
	if newUsed != used || newReserved != reserved || provider.submissions.Load() != 1 {
		t.Fatal("download restarted processing or changed quota")
	}
}

func TestMySQLVideoWaitingDoesNotExhaustRetriesAndHasDeadline(t *testing.T) {
	db := photoJournalDB(t)
	u, target := videoJournalUpload(t, db)
	r, ctx := NewMySQLUploads(db), context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	provider := &journalVideoProvider{}
	service := videoJournalService(t, r, &now, provider)
	worker, _ := NewVideoWorker(oneVideoQueue{r, target.ID}, service, journalVideoSpec(u).QueueID, 1)
	for range 20 {
		batch, err := worker.RunOnce(ctx)
		if err != nil || batch.Waiting != 1 || batch.Failed != 0 {
			t.Fatal("ordinary waiting classified as failure", batch, err)
		}
		now = now.Add(2 * time.Minute)
	}
	j, err := r.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if err != nil || j.Failures != 0 || provider.inspections.Load() != 1 || provider.submissions.Load() != 1 || provider.polls.Load() != 20 {
		t.Fatal("waiting repeated submission or spent retries", err)
	}
	now = j.Deadline.Add(time.Second)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	j, _ = r.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if j.State != "attention" || provider.polls.Load() != 20 {
		t.Fatal("expired task continued provider calls")
	}
}

func TestMySQLVideoBudgetBeforePaidSubmissionAndReceiptQuotaRollback(t *testing.T) {
	db := photoJournalDB(t)
	u, target := videoJournalUpload(t, db)
	r, ctx := NewMySQLUploads(db), context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	j, err := r.ClaimVideoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), now)
	if err != nil {
		t.Fatal(err)
	}
	spec := journalVideoSpec(u)
	spec.MaxBytes = u.DerivedBudget + 1
	if _, fresh, err := r.BeginVideoSubmission(ctx, j, spec, now); !errors.Is(err, ErrQuota) || fresh {
		t.Fatal("under-reserved paid task admitted", err)
	}
	unconsumed, _ := r.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if unconsumed.Spec != nil || !unconsumed.SubmittedAt.IsZero() {
		t.Fatal("failed quota check consumed the attempt")
	}
	spec = journalVideoSpec(u)
	j, fresh, err := r.BeginVideoSubmission(ctx, j, spec, now)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	j, err = r.AttachVideoSubmission(ctx, j, "j"+strings.Repeat("b", 32), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE album_storage_accounts SET reserved_bytes=0 WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	result := journalVideoResult(j)
	if _, err := r.CompleteVideoJob(ctx, j, result, now); !errors.Is(err, ErrQuota) {
		t.Fatal("missing reserve still published", err)
	}
	pending, _ := r.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if pending.Result != nil || pending.State != "running" {
		t.Fatal("receipt escaped rolled-back transaction")
	}
	if _, err := db.Exec(`UPDATE album_storage_accounts SET reserved_bytes=? WHERE owner_id=?`, u.DerivedBudget, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CompleteVideoJob(ctx, j, result, now); err != nil {
		t.Fatal("read-only verified result could not be recommitted", err)
	}
}
