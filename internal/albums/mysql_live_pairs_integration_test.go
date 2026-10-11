package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func livePairJournalFixture(t *testing.T, db *sql.DB) (Upload, LivePairDescriptor) {
	t.Helper()
	u := liveJournalUpload(t, db, true)
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM album_live_pairs WHERE owner_id=?`, u.OwnerID); err != nil {
			t.Error(err)
		}
	})
	r, ctx, now := NewMySQLUploads(db), context.Background(), time.Now().UTC().Truncate(time.Microsecond)
	jobs, err := r.PendingPhotoJobs(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.OwnerID != u.OwnerID {
			continue
		}
		spec := PhotoWriteSpec{OwnerID: u.OwnerID, AttemptID: uuid.NewString(), Preset: job.Preset, SizeBytes: 4096, SHA256: strings.Repeat("a", 64), Width: 800, Height: 600}
		if job.Preset == PhotoGridJPEG {
			spec.Width, spec.Height = 480, 270
		}
		p, _, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.CompletePhotoWrite(ctx, p, photoJournalReceipt(spec), now); err != nil {
			t.Fatal(err)
		}
		claimed, err := r.ClaimPhotoJob(ctx, u.OwnerID, job.ID, uuid.NewString(), now)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.FinishPhotoJob(ctx, claimed, true, false, now); err != nil {
			t.Fatal(err)
		}
	}
	j, err := r.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if err != nil {
		t.Fatal(err)
	}
	j, err = r.ClaimVideoJob(ctx, u.OwnerID, j.ID, uuid.NewString(), now)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := videoSource(AssetVersion{Manifest: u.Manifest, Objects: u.Objects})
	spec := VideoTranscodeSpec{Source: source, Profile: VideoProfile{1920, 1080, 1000, 30, 2}, MaxBytes: 64 << 10, QueueID: "p" + strings.Repeat("d", 32), LivePhotoTiming: u.Manifest.LivePhotoTiming}
	j, _, err = r.BeginVideoSubmission(ctx, j, spec, now)
	if err != nil {
		t.Fatal(err)
	}
	j, err = r.AttachVideoSubmission(ctx, j, "j"+strings.Repeat("b", 32), now)
	if err != nil {
		t.Fatal(err)
	}
	j, err = r.CompleteVideoJob(ctx, j, journalVideoResult(j), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now); err != nil {
		t.Fatal(err)
	}
	p, err := r.PhotoWrite(ctx, u.OwnerID, u.ID, PhotoDailyJPEG)
	if err != nil {
		t.Fatal(err)
	}
	return u, pairDescriptor(p, j)
}
func livePairTestService(t *testing.T, r *MySQLUploads, now *time.Time, transport cosTestTransport) *UploadService {
	t.Helper()
	objects := cosFixture(t, transport)
	objects.now = func() time.Time { return *now }
	s, err := NewUploadService(r, objects, UploadPolicy{MaxOriginalBytes: 1 << 30, MaxDerivedBytes: 1 << 20, UploadTTL: time.Hour, VerificationLease: 10 * time.Minute}, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestMySQLLivePairReservesOnceAfterComponentFinalizationAndFencesEverySeal(t *testing.T) {
	db := photoJournalDB(t)
	u, descriptor := livePairJournalFixture(t, db)
	r, ctx, now := NewMySQLUploads(db), context.Background(), time.Now().UTC().Truncate(time.Microsecond)
	s := livePairTestService(t, r, &now, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected provider call"); return nil, nil })
	usedBefore, reservedBefore := photoJournalBalance(t, db, u.OwnerID)
	if reservedBefore != 0 {
		t.Fatal("components not finalized")
	}
	request := uuid.NewString()
	var group sync.WaitGroup
	ids := make(chan string, 20)
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			pair, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, request, descriptor)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- pair.ID
		}()
	}
	group.Wait()
	close(ids)
	var id string
	for value := range ids {
		if id != "" && id != value {
			t.Fatal("duplicate active reservation")
		}
		id = value
	}
	size := descriptor.Photo.SizeBytes + descriptor.Movie.SizeBytes
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != usedBefore || reserved != size {
		t.Fatal("reservation not once", used, reserved)
	}
	changed := descriptor
	changed.Pair.Identifier = uuid.NewString()
	if _, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), changed); !errors.Is(err, ErrConflict) {
		t.Fatal("other device overwrote pending pair", err)
	}
	if _, err := r.LivePairByID(ctx, uuid.NewString(), id); !errors.Is(err, ErrNotFound) {
		t.Fatal("owner scope", err)
	}
	if _, err := s.SubmitLivePair(ctx, u.OwnerID, id); err != nil {
		t.Fatal(err)
	}
	won := make(chan LivePairUpload, 20)
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			pair, err := r.ClaimLivePair(ctx, u.OwnerID, id, uuid.NewString(), now, time.Minute)
			if err == nil {
				won <- pair
			} else if !errors.Is(err, ErrLease) {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	close(won)
	if len(won) != 1 {
		t.Fatal("duplicate worker lease", len(won))
	}
	first := <-won
	var permissions atomic.Int32
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, fresh, err := r.BeginLivePairSeal(ctx, first, "photo", now)
			if err != nil {
				t.Error(err)
			}
			if fresh {
				permissions.Add(1)
			}
		}()
	}
	group.Wait()
	if permissions.Load() != 1 {
		t.Fatal("COPY permission not once")
	}
	secondNow := first.LeaseUntil.Add(time.Second)
	reopened := NewMySQLUploads(db)
	second, err := reopened.ClaimLivePair(ctx, u.OwnerID, id, uuid.NewString(), secondNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := reopened.BeginLivePairSeal(ctx, second, "photo", secondNow); err != nil || fresh {
		t.Fatal("restart repeated COPY", err)
	}
	if _, _, err := r.BeginLivePairSeal(ctx, first, "motion", secondNow); !errors.Is(err, ErrLease) {
		t.Fatal("stale lease copied", err)
	}
	if _, err := reopened.CompleteLivePair(ctx, second, secondNow); !errors.Is(err, ErrLease) {
		t.Fatal("published without both byte receipts", err)
	}
	if _, err := s.LivePairReadGrant(ctx, u.OwnerID, u.ID); !errors.Is(err, ErrLivePairNotReady) {
		t.Fatal("partial pair readable", err)
	}
}
func TestMySQLLivePairRecoversLostCopyResponseWithoutRepeatingAndPublishesPinnedRead(t *testing.T) {
	db := photoJournalDB(t)
	u, descriptor := livePairJournalFixture(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var copies atomic.Int32
	archived := map[string]bool{}
	lost := true
	transport := func(request *http.Request) (*http.Response, error) {
		part := "photo"
		data := "paired-jpeg"
		if strings.HasSuffix(request.URL.Path, "/motion") {
			part = "motion"
			data = "paired-quicktime"
		}
		version := "sealed-" + part
		switch request.Method {
		case http.MethodHead:
			if strings.HasPrefix(request.URL.Path, "/albums/live-pairs/") && !archived[part] {
				return nil, io.ErrUnexpectedEOF
			}
			if strings.Contains(request.URL.Path, "/staging/") {
				version = "stage-" + part
			}
			response := cosResponse(request, "", version)
			response.ContentLength = int64(len(data))
			return response, nil
		case http.MethodPut:
			copies.Add(1)
			archived[part] = true
			if part == "motion" && lost {
				lost = false
				return nil, io.ErrUnexpectedEOF
			}
			return cosResponse(request, `<CopyObjectResult><ETag>ok</ETag></CopyObjectResult>`, version), nil
		case http.MethodGet:
			if request.URL.Query().Get("versionId") != version {
				t.Error("read not immutable")
			}
			return cosResponse(request, data, version), nil
		default:
			t.Fatal("unexpected method", request.Method)
			return nil, nil
		}
	}
	s := livePairTestService(t, r, &now, transport)
	before, _ := photoJournalBalance(t, db, u.OwnerID)
	pair, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := s.LivePairGrants(ctx, u.OwnerID, pair.ID)
	if err != nil || len(grants) != 2 {
		t.Fatal(err)
	}
	if _, err := s.SubmitLivePair(ctx, u.OwnerID, pair.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyLivePair(ctx, u.OwnerID, pair.ID); !errors.Is(err, ErrLivePairNotReady) {
		t.Fatal("lost response published", err)
	}
	size := descriptor.Photo.SizeBytes + descriptor.Movie.SizeBytes
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != before || reserved != size {
		t.Fatal("unverified charged as complete")
	}
	now = now.Add(2 * time.Minute)
	s = livePairTestService(t, NewMySQLUploads(db), &now, transport)
	worker, _ := NewLivePairWorker(NewMySQLUploads(db), s, 10)
	batch, err := worker.RunOnce(ctx)
	if err != nil || batch.Verified != 1 || copies.Load() != 2 {
		t.Fatal("recovery COPY repeated", batch, err, copies.Load())
	}
	used, reserved = photoJournalBalance(t, db, u.OwnerID)
	if used != before+size || reserved != 0 {
		t.Fatal("pair quota not settled once", used, reserved)
	}
	ready, err := s.LivePair(ctx, u.OwnerID, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := r.CompleteLivePair(ctx, ready, now); err != nil {
			t.Fatal(err)
		}
	}
	changed := descriptor
	changed.Pair.Identifier = uuid.NewString()
	existing, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), changed)
	if err != nil || existing.ID != pair.ID || existing.Descriptor != descriptor {
		t.Fatal("canonical race", err)
	}
	grant, err := s.LivePairReadGrant(ctx, u.OwnerID, u.ID)
	if err != nil || grant.ReplacementEligible || grant.Movie.ContentType != "video/quicktime" || grant.Photo.ContentType != "image/jpeg" || grant.Descriptor != descriptor {
		t.Fatal("bad pair read", err)
	}
	encoded, _ := json.Marshal(ready)
	if strings.Contains(string(encoded), "https://") || strings.Contains(string(encoded), "sealed-") || strings.Contains(string(encoded), "albums/live-pairs/") {
		t.Fatal("provider locations persisted in wire state")
	}
	router, _ := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", u.OwnerID}}, s)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "https://albums.example/v1/albums/library/versions/"+u.ID+"/live-pair/read-grant", nil))
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("HTTP read", response.Code)
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "https://albums.example/v1/albums/live-pair-uploads/"+pair.ID+"/submit", strings.NewReader(`{"state":"verified"}`)))
	if response.Code != 422 {
		t.Fatal("client proof accepted", response.Code)
	}
}
func TestMySQLLivePairRejectsSourceReceiptSubstitutionAndRetainsEscrowOnHashMismatch(t *testing.T) {
	db := photoJournalDB(t)
	u, descriptor := livePairJournalFixture(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	s := livePairTestService(t, r, &now, func(request *http.Request) (*http.Response, error) {
		part := "photo"
		data := "paired-jpeg"
		if strings.HasSuffix(request.URL.Path, "/motion") {
			part = "motion"
			data = "paired-quicktime"
		}
		version := "sealed-" + part
		switch request.Method {
		case http.MethodHead:
			response := cosResponse(request, "", "stage-"+part)
			response.ContentLength = int64(len(data))
			return response, nil
		case http.MethodPut:
			return cosResponse(request, `<CopyObjectResult><ETag>ok</ETag></CopyObjectResult>`, version), nil
		case http.MethodGet:
			return cosResponse(request, strings.Repeat("x", len(data)), version), nil
		}
		return nil, io.ErrUnexpectedEOF
	})
	bad := descriptor
	bad.SourcePhoto.SHA256 = strings.Repeat("f", 64)
	if _, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), bad); !errors.Is(err, ErrLivePhotoNotReady) {
		t.Fatal("substituted component", err)
	}
	before, reserve := photoJournalBalance(t, db, u.OwnerID)
	if reserve != 0 {
		t.Fatal("bad source reserved quota")
	}
	pair, err := s.CreateLivePair(ctx, u.OwnerID, u.ID, uuid.NewString(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitLivePair(ctx, u.OwnerID, pair.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyLivePair(ctx, u.OwnerID, pair.ID); !errors.Is(err, ErrIncompleteBackup) {
		t.Fatal("size-only verification", err)
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != before || reserved != descriptor.Photo.SizeBytes+descriptor.Movie.SizeBytes {
		t.Fatal("invalid bytes settled escrow")
	}
	if _, err := s.LivePairReadGrant(ctx, u.OwnerID, u.ID); !errors.Is(err, ErrLivePairNotReady) {
		t.Fatal("bad hash signed", err)
	}
	// Exhaustion is durable attention, with escrow intact. It must never grant a
	// fresh COPY or silently release possibly still-billed versions.
	for range 8 {
		now = now.Add(2 * time.Minute)
		_, _ = s.VerifyLivePair(ctx, u.OwnerID, pair.ID)
	}
	current, err := r.LivePair(ctx, u.OwnerID, u.ID)
	if err != nil || current.State != "attention" {
		t.Fatal("unbounded recovery", current.State, err)
	}
}
