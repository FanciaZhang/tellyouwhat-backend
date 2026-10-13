package albums

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

func TestMySQLAccountMediaObservationWorkerRunsAllQueuesWithoutMutations(t *testing.T) {
	f, r, photo := deletionPhotoFixture(t, true)
	ctx := context.Background()
	_, _, _, owner, manifest := uploadFixture(t)
	digest, _ := manifest.Digest()
	original := Upload{ID: uuid.NewString(), OwnerID: owner, RequestID: uuid.NewString(), Manifest: manifest, Digest: digest, State: Uploading, OriginalBytes: manifest.Resources[0].SizeBytes, DerivedBudget: 4096, ExpiresAt: f.now.Add(time.Hour)}
	if err := r.ProvisionAccount(ctx, owner, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reserve(ctx, original); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, owner, original.ID, f.now); err != nil {
		t.Fatal(err)
	}
	original, err := r.Claim(ctx, owner, original.ID, uuid.NewString(), f.now, f.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := r.BeginOriginalSeal(ctx, original, "photo", f.now); err != nil || !fresh {
		t.Fatal(err)
	}
	deleteProtocolFixtureOwner(t, f, owner)
	source, descriptor := livePairJournalFixture(t, f.r.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := livePairTestService(t, r, &now, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected setup network"); return nil, nil })
	pair, err := service.CreateLivePair(ctx, source.OwnerID, source.ID, uuid.NewString(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.SubmitLivePair(ctx, source.OwnerID, pair.ID); err != nil {
		t.Fatal(err)
	}
	pair, err = r.ClaimLivePair(ctx, source.OwnerID, pair.ID, uuid.NewString(), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"photo", "motion"} {
		pair, _, err = r.BeginLivePairSeal(ctx, pair, part, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = r.DeferLivePair(ctx, pair, now); err != nil {
		t.Fatal(err)
	}
	deleteProtocolFixtureOwner(t, f, source.OwnerID)
	video, err := r.VideoJob(ctx, source.OwnerID, source.ID, VideoDaily1080)
	if err != nil {
		t.Fatal(err)
	}
	photoKey, _ := photo.record.Spec.key()
	pairPhotoKey, _ := pair.key("photo", false)
	pairMovieKey, _ := pair.key("motion", false)
	heads := map[string]struct {
		version string
		size    int64
	}{
		"/" + sealedKey(original, "photo"): {"original-copy", original.OriginalBytes},
		"/" + photoKey:                     {"photo-put", photo.record.Spec.SizeBytes},
		"/" + pairPhotoKey:                 {"pair-photo", pair.Descriptor.Photo.SizeBytes},
		"/" + pairMovieKey:                 {"pair-movie", pair.Descriptor.Movie.SizeBytes},
	}
	calls := 0
	objects := cosFixture(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method == http.MethodHead && req.URL.RawQuery == "" {
			object, ok := heads[req.URL.Path]
			if !ok {
				t.Fatal("foreign metadata query")
			}
			response := cosResponse(req, "", object.version)
			response.ContentLength = object.size
			return response, nil
		}
		if req.Method == http.MethodGet && req.URL.Path == "/jobs/"+video.ProviderJobID {
			return videoResponse(t, req, &cos.DescribeMediaProcessJobResult{JobsDetail: videoDetail(video, "Success")}), nil
		}
		t.Fatal("unexpected provider mutation or media download", req.Method, req.URL.Path)
		return nil, nil
	})
	worker, err := NewAccountMediaObservationWorker(r, objects, 10, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Check(ctx); err != nil {
		t.Fatal(err)
	}
	before, reservedBefore := photoJournalBalance(t, f.r.db, photo.account)
	batch, err := worker.RunOnce(ctx)
	if err != nil || batch != (AccountMediaObservationBatch{Acknowledged: 4}) || calls != 5 {
		t.Fatal("queues not observed", batch, err, calls)
	}
	version, consumed, err := r.OriginalSeal(ctx, original, "photo")
	if err != nil || !consumed || version != "original-copy" {
		t.Fatal("original receipt missing", err)
	}
	assertPhotoDeletionObservation(t, r, photo, "photo-put")
	assertDeletionLivePair(t, r, livePairDeletionCheck{account: pair.OwnerID, id: pair.ID}, "pair-photo", "pair-movie")
	var acknowledged bool
	if err = f.r.db.QueryRow(`SELECT acknowledged_at IS NOT NULL FROM album_deletion_video_jobs WHERE job_id=?`, video.ID).Scan(&acknowledged); err != nil || !acknowledged {
		t.Fatal("video terminal receipt missing", err)
	}
	used, reserved := photoJournalBalance(t, f.r.db, photo.account)
	if used != before || reserved != reservedBefore {
		t.Fatal("observation published preview")
	}
	// A finished queue is not re-observed and the worker stops on cancellation.
	run, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	reports := 0
	err = worker.Run(run, time.Second, func(batch AccountMediaObservationBatch, available bool) {
		reports++
		if !available || batch != (AccountMediaObservationBatch{}) {
			t.Error("completed queues repeated", batch, available)
		}
		cancel()
	})
	if !errors.Is(err, context.Canceled) || reports != 1 || calls != 5 {
		t.Fatal("worker did not stop cleanly", err, reports, calls)
	}
	// Missing migration fails readiness and reports unavailable rather than success.
	if _, err = f.r.db.Exec(`ALTER TABLE album_photo_writes RENAME COLUMN deletion_check_after TO hidden_deletion_check_after`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := f.r.db.Exec(`ALTER TABLE album_photo_writes RENAME COLUMN hidden_deletion_check_after TO deletion_check_after`); err != nil {
			t.Error(err)
		}
	}()
	if err = worker.Check(ctx); err == nil {
		t.Fatal("missing migration accepted")
	}
	run, cancel = context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	reports = 0
	err = worker.Run(run, time.Second, func(_ AccountMediaObservationBatch, available bool) {
		reports++
		if available {
			t.Error("database error reported healthy")
		}
		cancel()
	})
	if !errors.Is(err, context.Canceled) || reports != 1 || calls != 5 {
		t.Fatal("failed pass retried cloud writes", err, reports, calls)
	}
}
