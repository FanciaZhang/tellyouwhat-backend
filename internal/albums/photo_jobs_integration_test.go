package albums

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ownPhotoJobs(t *testing.T, r *MySQLUploads, owner string) []PhotoJob {
	t.Helper()
	rows, err := r.db.Query(`SELECT `+photoJobColumns+` FROM album_photo_jobs WHERE owner_id=? ORDER BY preset`, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var jobs []PhotoJob
	for rows.Next() {
		j, err := scanPhotoJob(rows)
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return jobs
}
func TestMySQLPhotoQueuePublishesOnceClaimsAndFencesLeases(t *testing.T) {
	db := photoJournalDB(t)
	u, _ := photoJournalUpload(t, db)
	ctx := context.Background()
	r := NewMySQLUploads(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := r.CheckPhotoSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := r.Complete(ctx, u.OwnerID, u.ID, "", nil, now); err != nil {
			t.Fatal(err)
		}
	}
	jobs := ownPhotoJobs(t, r, u.OwnerID)
	if len(jobs) != 2 {
		t.Fatal("not exactly two tasks", len(jobs))
	}
	target := jobs[0]
	var group sync.WaitGroup
	won := make(chan PhotoJob, 20)
	failures := make(chan error, 20)
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			j, err := r.ClaimPhotoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), now)
			if err == nil {
				won <- j
			} else {
				failures <- err
			}
		}()
	}
	group.Wait()
	close(won)
	close(failures)
	if len(won) != 1 {
		t.Fatal("duplicate generation lease", len(won))
	}
	first := <-won
	for err := range failures {
		if !errors.Is(err, ErrLease) {
			t.Fatal(err)
		}
	}
	if _, err := r.ClaimPhotoJob(ctx, uuid.NewString(), target.ID, uuid.NewString(), now); !errors.Is(err, ErrLease) {
		t.Fatal("cross-owner claim", err)
	}
	afterExpiry := first.LeaseUntil.Add(time.Second)
	second, err := r.ClaimPhotoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), afterExpiry)
	if err != nil || second.Attempts != 2 {
		t.Fatal("expired lease not recovered", err)
	}
	if err := r.FinishPhotoJob(ctx, first, true, false, afterExpiry); !errors.Is(err, ErrLease) {
		t.Fatal("stale worker acknowledged task", err)
	}
	if err := r.FinishPhotoJob(ctx, second, false, false, afterExpiry); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimPhotoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), afterExpiry.Add(59*time.Second)); !errors.Is(err, ErrLease) {
		t.Fatal("retry ignored backoff", err)
	}
	third, err := r.ClaimPhotoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), afterExpiry.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.FinishPhotoJob(ctx, third, false, true, afterExpiry.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimPhotoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), afterExpiry.Add(24*time.Hour)); !errors.Is(err, ErrLease) {
		t.Fatal("terminal task reran", err)
	}
}

type onePhotoQueue struct {
	*MySQLUploads
	id string
}

func (q onePhotoQueue) PendingPhotoJobs(ctx context.Context, now time.Time, limit int) ([]PhotoJob, error) {
	jobs, err := q.MySQLUploads.PendingPhotoJobs(ctx, now, 100)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.ID == q.id {
			return []PhotoJob{j}, nil
		}
	}
	return nil, nil
}

type queuedPhotoProvider struct {
	*objectTestStore
	candidate               PhotoCandidate
	renders, puts, recovers atomic.Int32
	entered, release        chan struct{}
	lost                    bool
}

func (p *queuedPhotoProvider) RenderPhotoCandidate(ctx context.Context, _ SealedObject, _ PhotoPreset) (PhotoCandidate, error) {
	p.renders.Add(1)
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return PhotoCandidate{}, ctx.Err()
		}
	}
	return p.candidate, nil
}
func (p *queuedPhotoProvider) StorePhotoCandidateOnce(_ context.Context, spec PhotoWriteSpec, _ PhotoCandidate) (SealedObject, error) {
	p.puts.Add(1)
	if p.lost {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	return photoJournalReceipt(spec), nil
}
func (p *queuedPhotoProvider) ReconcilePhotoCandidate(_ context.Context, spec PhotoWriteSpec) (SealedObject, error) {
	p.recovers.Add(1)
	return photoJournalReceipt(spec), nil
}
func TestMySQLPhotoWorkersSerializeGenerationAndResumeLostWrites(t *testing.T) {
	db := photoJournalDB(t)
	u, c := photoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC()
	_, _, objects, _, _ := uploadFixture(t)
	provider := &queuedPhotoProvider{objectTestStore: objects, candidate: c, entered: make(chan struct{}, 1), release: make(chan struct{}), lost: true}
	service, err := NewUploadService(r, provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	jobs := ownPhotoJobs(t, r, u.OwnerID)
	var target PhotoJob
	for _, j := range jobs {
		if j.Preset == PhotoGridPNG {
			target = j
		}
	}
	queue := onePhotoQueue{r, target.ID}
	first, _ := NewPhotoWorker(queue, service, 1)
	second, _ := NewPhotoWorker(queue, service, 1)
	done := make(chan error, 1)
	go func() {
		batch, err := first.RunOnce(ctx)
		if err == nil && batch.Failed != 1 {
			err = errors.New("lost write not reported")
		}
		done <- err
	}()
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start render")
	}
	batch, err := second.RunOnce(ctx)
	if err != nil || batch.Stored+batch.Failed != 0 || provider.renders.Load() != 1 {
		t.Fatal("second worker repeated in-flight generation", batch, err)
	}
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	now = now.Add(29 * time.Second)
	if batch, err := second.RunOnce(ctx); err != nil || batch.Stored+batch.Failed != 0 {
		t.Fatal("no backoff", batch, err)
	}
	now = now.Add(time.Second)
	batch, err = second.RunOnce(ctx)
	if err != nil || batch.Stored != 1 {
		t.Fatal("lost write did not resume", batch, err)
	}
	if provider.renders.Load() != 1 || provider.puts.Load() != 1 || provider.recovers.Load() != 1 {
		t.Fatal("recovery repeated generation or PUT")
	}
	if batch, err := second.RunOnce(ctx); err != nil || batch.Stored+batch.Failed != 0 {
		t.Fatal("completed task still runnable", batch, err)
	}
}
func TestMySQLPhotoWorkerCrashLimitDoesNotLoopForever(t *testing.T) {
	db := photoJournalDB(t)
	u, _ := photoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC()
	target := ownPhotoJobs(t, r, u.OwnerID)[0]
	for attempt := 1; attempt <= 8; attempt++ {
		job, err := r.ClaimPhotoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), now)
		if err != nil || job.Attempts != attempt {
			t.Fatal("claim", attempt, err)
		}
		now = job.LeaseUntil.Add(time.Second)
	}
	if _, err := r.ClaimPhotoJob(ctx, u.OwnerID, target.ID, uuid.NewString(), now); !errors.Is(err, ErrLease) {
		t.Fatal("crash retries unbounded", err)
	}
	for _, j := range ownPhotoJobs(t, r, u.OwnerID) {
		if j.ID == target.ID && j.State != "attention" {
			t.Fatal("exhausted task not visible as attention")
		}
	}
}
