package albums

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func timedLiveManifest() Manifest {
	m := liveManifest()
	m.Resources[0].Descriptor = &ResourceDescriptor{1, "still.HEIC", "public.heic"}
	m.Resources[1].Descriptor = &ResourceDescriptor{9, "motion.MOV", "com.apple.quicktime-movie"}
	m.Metadata = &AssetMetadata{PixelWidth: 1920, PixelHeight: 1080, MediaSubtypes: 8}
	m.LivePhotoTiming = &LivePhotoTiming{"photo", "motion", 500000, 1000000}
	return m
}

func TestLiveTimingIsCurrentPairBoundAndLegacyDigestIsUnchanged(t *testing.T) {
	legacy := liveManifest()
	sort.Slice(legacy.Resources, func(i, j int) bool { return legacy.Resources[i].ID < legacy.Resources[j].ID })
	data, _ := json.Marshal(legacy)
	want := sha256.Sum256(data)
	digest, err := legacy.Digest()
	if err != nil || digest != hex.EncodeToString(want[:]) || strings.Contains(string(data), "livePhotoTiming") {
		t.Fatal("legacy identity changed", err)
	}
	m := timedLiveManifest()
	before, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	m.LivePhotoTiming.PhotoTimeMicroseconds++
	after, _ := m.Digest()
	if before == after {
		t.Fatal("timing not digest-bound")
	}
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.LivePhotoTiming.PhotoTimeMicroseconds = -1 },
		func(m *Manifest) { m.LivePhotoTiming.PhotoTimeMicroseconds = m.LivePhotoTiming.DurationMicroseconds },
		func(m *Manifest) { m.LivePhotoTiming.DurationMicroseconds = 3600000001 },
		func(m *Manifest) { m.LivePhotoTiming.PhotoResourceID = "motion" },
		func(m *Manifest) { m.LivePhotoTiming.VideoResourceID = "missing" },
		func(m *Manifest) { m.Resources[0].Descriptor = nil; m.Resources[1].Descriptor = nil },
		func(m *Manifest) { m.Metadata = nil },
		func(m *Manifest) { m.Metadata.MediaSubtypes = 0 },
	} {
		m := timedLiveManifest()
		mutate(&m)
		if !errors.Is(m.Validate(), ErrInvalidManifest) {
			t.Fatal("accepted invalid current pair")
		}
	}
	m = timedLiveManifest()
	m.Edited = true
	m.Resources = append(m.Resources, Resource{"display", CurrentPhoto, 80, strings.Repeat("c", 64), &ResourceDescriptor{5, "display.jpg", "public.jpeg"}}, Resource{"display-motion", CurrentVideo, 120, strings.Repeat("d", 64), &ResourceDescriptor{10, "display.mov", "com.apple.quicktime-movie"}})
	if m.Validate() == nil {
		t.Fatal("unedited timing bound to edited manifest")
	}
	m.LivePhotoTiming.PhotoResourceID, m.LivePhotoTiming.VideoResourceID = "display", "display-motion"
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.Resources[3].Descriptor.PhotoKitType = 6
	if m.Validate() == nil {
		t.Fatal("ordinary video render used as paired video")
	}
}

func TestLiveSourceAndProfileRequireSupportedBoundTiming(t *testing.T) {
	m := timedLiveManifest()
	v := AssetVersion{Manifest: m, Objects: []SealedObject{{ResourceID: "motion", SizeBytes: 200, SHA256: m.Resources[1].SHA256}}}
	if got, err := videoSource(v); err != nil || got.ResourceID != "motion" {
		t.Fatal("paired source not selected", err)
	}
	if got := photoJobPresets(m); len(got) != 2 || got[0] != PhotoGridJPEG || got[1] != PhotoDailyJPEG {
		t.Fatal("daily pair needs JPEG components")
	}
	p := VideoProfile{1920, 1080, 1000, 30, 2}
	if !livePhotoProfileMatches(m, p) {
		t.Fatal("valid timing rejected")
	}
	p.DurationMilliseconds = 500
	if livePhotoProfileMatches(m, p) {
		t.Fatal("key photo outside derived duration")
	}
	p.DurationMilliseconds = 1400
	if livePhotoProfileMatches(m, p) {
		t.Fatal("provider timeline drift accepted")
	}
	for _, change := range []func(*Manifest){func(m *Manifest) { m.LivePhotoTiming = nil }, func(m *Manifest) { m.Metadata.MediaSubtypes = 10 }} {
		m := timedLiveManifest()
		change(&m)
		v.Manifest = m
		if _, err := videoSource(v); !errors.Is(err, ErrVideoUnsupported) {
			t.Fatal("legacy or special Live Photo processed", err)
		}
	}
}

func TestLiveSpecSurvivesJSONReopenAndRejectsShortenedMovieInEveryReceiptPath(t *testing.T) {
	j := videoJobFixture()
	j.Spec.LivePhotoTiming = &LivePhotoTiming{"photo", j.Spec.Source.ResourceID, 999999, 1000000}
	j.Spec.Profile.DurationMilliseconds = 1000
	data, err := json.Marshal(j.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var reopened VideoTranscodeSpec
	if err := json.Unmarshal(data, &reopened); err != nil {
		t.Fatal(err)
	}
	if !j.Spec.matches(reopened) {
		t.Fatal("identical timing depends on pointer identity after reopen")
	}
	result := journalVideoResult(j)
	if err := j.validateResult(result); err != nil {
		t.Fatal(err)
	}
	result.Profile.DurationMilliseconds = 990
	if !errors.Is(j.validateResult(result), ErrVideoProcessing) {
		t.Fatal("a near-end key photo fell outside the recovered movie")
	}
	reopened.LivePhotoTiming.PhotoTimeMicroseconds--
	if j.Spec.matches(reopened) {
		t.Fatal("changed timing ignored on replay")
	}
}

type liveReadRepository struct {
	*videoReadRepository
	record PhotoWriteRecord
}

func (r *liveReadRepository) PhotoWrite(_ context.Context, owner, version string, preset PhotoPreset) (PhotoWriteRecord, error) {
	if owner != r.owner || version != r.version.VersionID || preset != PhotoDailyJPEG {
		return PhotoWriteRecord{}, ErrNotFound
	}
	return r.record, nil
}
func (*liveReadRepository) BeginPhotoWrite(context.Context, string, string, PhotoWriteSpec, time.Time) (PhotoWriteRecord, bool, error) {
	panic("read cannot write")
}
func (*liveReadRepository) CompletePhotoWrite(context.Context, PhotoWriteRecord, SealedObject, time.Time) (PhotoWriteRecord, error) {
	panic("read cannot complete")
}

type liveReadSigner struct{ *videoReadSigner }

func (s *liveReadSigner) AuthorizePhotoPreviewRead(context.Context, PhotoWriteRecord, time.Time) (string, error) {
	s.calls++
	return "https://storage.example/live-still", nil
}

func liveReadFixture(t *testing.T) (*UploadService, *liveReadRepository, *liveReadSigner) {
	s, r, signer := videoReadFixture(t)
	m := timedLiveManifest()
	r.version.Manifest = m
	r.version.AssetID = m.AssetID
	r.version.ManifestDigest, _ = m.Digest()
	r.version.OriginalBytes = 300
	r.version.Objects = nil
	for _, resource := range m.Resources {
		r.version.Objects = append(r.version.Objects, SealedObject{resource.ID, sealedKey(Upload{ID: r.version.VersionID, OwnerID: r.owner}, resource.ID), "original-version", resource.SizeBytes, resource.SHA256})
	}
	r.job.ManifestDigest = r.version.ManifestDigest
	r.job.Spec.Source = r.version.Objects[1]
	r.job.Spec.LivePhotoTiming = m.LivePhotoTiming
	r.job.Spec.Profile.DurationMilliseconds = 1000
	r.job.Result.Profile = r.job.Spec.Profile
	record := PhotoWriteRecord{VersionID: r.version.VersionID, ManifestDigest: r.version.ManifestDigest, Spec: PhotoWriteSpec{OwnerID: r.owner, AttemptID: uuid.NewString(), Preset: PhotoDailyJPEG, SizeBytes: 123, SHA256: strings.Repeat("a", 64), Width: 800, Height: 600}, ObjectVersion: "daily-version"}
	repository := &liveReadRepository{r, record}
	object := &liveReadSigner{signer}
	s.repository, s.objects = repository, object
	return s, repository, object
}

func TestLiveCombinedGrantRequiresBothOwnedCompletedComponentsBeforeSigning(t *testing.T) {
	s, r, signer := liveReadFixture(t)
	grant, err := s.LivePhotoPreviewReadGrant(context.Background(), r.owner, r.version.VersionID)
	if err != nil || signer.calls != 2 || grant.ReplacementEligible || grant.Photo.ReplacementEligible || grant.Video.ReplacementEligible || grant.Timing != *r.version.Manifest.LivePhotoTiming || grant.Photo.ManifestDigest != grant.ManifestDigest || grant.Video.ManifestDigest != grant.ManifestDigest {
		t.Fatal("wrong live grant", err)
	}
	for _, edit := range []func(*liveReadRepository){
		func(r *liveReadRepository) { r.record.ObjectVersion = "" },
		func(r *liveReadRepository) { r.job.State = "running"; r.job.Result = nil },
		func(r *liveReadRepository) { r.record.ManifestDigest = strings.Repeat("f", 64) },
		func(r *liveReadRepository) { r.job.Spec.Source.VersionID = "other-source" },
		func(r *liveReadRepository) { r.job.Result.Profile.DurationMilliseconds = 400 },
		func(r *liveReadRepository) { r.version.Objects = nil },
	} {
		s, r, signer := liveReadFixture(t)
		edit(r)
		if _, err := s.LivePhotoPreviewReadGrant(context.Background(), r.owner, r.version.VersionID); err == nil || signer.calls != 0 {
			t.Fatal("incomplete pair signed", err)
		}
	}
	router, err := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", r.owner}}, s)
	if err != nil {
		t.Fatal(err)
	}
	path := "https://albums.example/v1/albums/library/versions/" + r.version.VersionID + "/live-previews/daily/read-grant"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", path, nil))
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("not private", response.Code, response.Body.String())
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if err := doc.Components.Schemas["LivePhotoPreviewReadGrant"].Value.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"?owner=other"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("POST", path+suffix, nil))
		if response.Code < 400 {
			t.Fatal("unscoped read")
		}
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", path, strings.NewReader("{}")))
	if response.Code < 400 {
		t.Fatal("read accepted body")
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", strings.Replace(path, r.version.VersionID, uuid.NewString(), 1), nil))
	if response.Code != 404 {
		t.Fatal("other version readable", response.Code)
	}
}

func liveJournalUpload(t *testing.T, db *sql.DB, timing bool) Upload {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	r := NewMySQLUploads(db)
	owner := uuid.NewString()
	m := timedLiveManifest()
	m.Resources[0].SizeBytes = 2 << 20
	m.Resources[1].SizeBytes = 4 << 20
	if !timing {
		m.LivePhotoTiming = nil
	}
	digest, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	u := Upload{ID: uuid.NewString(), OwnerID: owner, RequestID: uuid.NewString(), Manifest: m, Digest: digest, State: Uploading, OriginalBytes: 6 << 20, DerivedBudget: 1 << 20, ExpiresAt: now.Add(time.Hour)}
	if err := r.ProvisionAccount(ctx, owner, 1<<30); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"album_derived_cleanups", "album_derived_finalizations", "album_photo_writes", "album_photo_jobs", "album_video_jobs", "album_asset_versions", "album_uploads", "album_storage_accounts"} {
			db.Exec("DELETE FROM "+table+" WHERE owner_id=?", owner)
		}
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
	var objects []SealedObject
	for _, resource := range m.Resources {
		objects = append(objects, SealedObject{resource.ID, sealedKey(u, resource.ID), "original-version", resource.SizeBytes, resource.SHA256})
	}
	recordFixtureSeals(t, r, u, token, objects, now)
	u, err = r.Complete(ctx, owner, u.ID, token, objects, now)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestMySQLLiveMovieWaitsForStillsAndFinalizesAllThreeComponents(t *testing.T) {
	db := photoJournalDB(t)
	u := liveJournalUpload(t, db, true)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	jobs := ownPhotoJobs(t, r, u.OwnerID)
	if len(jobs) != 2 {
		t.Fatal("missing still jobs")
	}
	target, err := r.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if err != nil {
		t.Fatal(err)
	}
	provider := &journalVideoProvider{ready: true, lost: true}
	s := videoJournalService(t, r, &now, provider)
	worker, _ := NewVideoWorker(oneVideoQueue{r, target.ID}, s, "p"+strings.Repeat("d", 32), 1)
	batch, err := worker.RunOnce(ctx)
	if err != nil || batch.Waiting != 1 || provider.inspections.Load() != 0 || provider.submissions.Load() != 0 {
		t.Fatal("paid movie started before stills", batch, err)
	}
	if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("closed before pair", err)
	}
	for _, job := range jobs {
		spec := PhotoWriteSpec{OwnerID: u.OwnerID, AttemptID: uuid.NewString(), Preset: job.Preset, SizeBytes: 4096, SHA256: strings.Repeat("a", 64), Width: 800, Height: 600}
		if job.Preset == PhotoGridJPEG {
			spec.Width, spec.Height = 480, 270
		}
		p, fresh, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, now)
		if err != nil || !fresh {
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
	if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("closed with movie waiting", err)
	}
	now = now.Add(2 * time.Minute)
	batch, err = worker.RunOnce(ctx)
	if err != nil || batch.Failed != 1 || provider.submissions.Load() != 1 {
		t.Fatal("lost movie not durable", batch, err)
	}
	current, err := r.VideoJob(ctx, u.OwnerID, u.ID, VideoDaily1080)
	if err != nil || current.Spec == nil || current.Spec.MaxBytes != u.DerivedBudget-8192 || current.Spec.Source.ResourceID != "motion" {
		t.Fatal("shared cap/source wrong", current, err)
	}
	now = now.Add(16 * time.Minute)
	reopened := NewMySQLUploads(db)
	s = videoJournalService(t, reopened, &now, provider)
	worker, _ = NewVideoWorker(oneVideoQueue{reopened, target.ID}, s, current.Spec.QueueID, 1)
	batch, err = worker.RunOnce(ctx)
	if err != nil || batch.Stored != 1 || provider.submissions.Load() != 1 || provider.discoveries.Load() != 1 {
		t.Fatal("restart resubmitted or lost pair", batch, err)
	}
	refs, err := reopened.PendingDerivedFinalizations(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range refs {
		if ref.OwnerID == u.OwnerID && ref.VersionID == u.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("three-component version not scheduled")
	}
	f, err := reopened.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now)
	if err != nil || f.RetainedBytes != 12288 || f.HeldBytes != 0 || f.ReleasedBytes != u.DerivedBudget-12288 {
		t.Fatal("wrong three-component settlement", f, err)
	}
	if _, err := NewMySQLUploads(db).FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != u.OriginalBytes+12288 || reserved != 0 {
		t.Fatal("quota duplicated or held", used, reserved)
	}
	legacy := liveJournalUpload(t, db, false)
	if _, err := r.VideoJob(ctx, legacy.OwnerID, legacy.ID, VideoDaily1080); !errors.Is(err, ErrNotFound) {
		t.Fatal("legacy live movie scheduled", err)
	}
}
