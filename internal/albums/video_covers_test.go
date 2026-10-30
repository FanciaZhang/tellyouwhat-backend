package albums

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCOSVideoCoverPinsVersionFitsCanvasAndRejectsChangedSource(t *testing.T) {
	for _, scenario := range []string{"landscape", "portrait", "short", "changed", "bad-jpeg", "too-large", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			j := videoJobFixture()
			source, profile := j.Spec.Source, j.Spec.Profile
			if scenario == "portrait" {
				profile.Width, profile.Height = 1080, 1920
			}
			if scenario == "short" {
				profile.DurationMilliseconds = 200
			}
			snapshots, heads := 0, 0
			objects := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/"+source.Key {
					t.Fatal("wrong source")
				}
				if r.Method == "HEAD" {
					heads++
					version := source.VersionID
					if scenario == "changed" && snapshots > 0 {
						version = "different-version"
					}
					res := cosResponse(r, "", version)
					res.ContentLength = source.SizeBytes
					return res, nil
				}
				if r.URL.Query().Get("versionId") != source.VersionID || r.URL.Fragment != "" {
					t.Fatal("source version not pinned")
				}
				if r.URL.Query().Get("ci-process") == "videoinfo" {
					return mediaInfoResponse(t, r, source.SizeBytes, profile, source.VersionID), nil
				}
				snapshots++
				q := r.URL.Query()
				if r.Method != "GET" || q.Get("ci-process") != "snapshot" || q.Get("format") != "jpg" || q.Get("mode") != "exactframe" || q.Get("rotate") != "auto" {
					t.Fatal("unbounded snapshot")
				}
				if scenario == "portrait" {
					if q.Get("height") != "512" || q.Has("width") {
						t.Fatal("portrait distorted")
					}
				} else if q.Get("width") != "512" || q.Has("height") {
					t.Fatal("landscape distorted")
				}
				expectedTime := "0.5"
				if scenario == "short" {
					expectedTime = "0.1"
				}
				if q.Get("time") != expectedTime {
					t.Fatal("time outside clip", q.Get("time"))
				}
				data := candidateImage(t, "jpeg", 11)
				if scenario == "bad-jpeg" {
					data = []byte("not jpeg")
				}
				if scenario == "too-large" {
					data = make([]byte, (2<<20)+1)
				}
				return cosResponse(r, string(data), source.VersionID), nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			candidate, err := objects.RenderVideoCover(ctx, source)
			success := scenario == "landscape" || scenario == "portrait" || scenario == "short"
			if success {
				if err != nil || candidate.Format != "jpeg" || candidate.Width != 11 || snapshots != 1 || heads != 3 {
					t.Fatal("wrong cover", err, snapshots, heads)
				}
			} else if err == nil {
				t.Fatal("invalid cover accepted")
			}
			if scenario == "cancelled" && snapshots != 0 {
				t.Fatal("cancelled render reached provider")
			}
		})
	}
}

type coverRenderer struct{ *photoRenderer }

func (r *coverRenderer) RenderVideoCover(_ context.Context, source SealedObject) (PhotoCandidate, error) {
	r.calls++
	r.selected = source
	return PhotoCandidate{Format: "jpeg"}, nil
}
func TestVideoCoverSelectsEditedMovieAndCannotBecomeDailyPhoto(t *testing.T) {
	for _, edited := range []bool{false, true} {
		service, repo, signer := videoReadFixture(t)
		renderer := &coverRenderer{&photoRenderer{objectTestStore: signer.objectTestStore}}
		service.objects = renderer
		expected := repo.version.Objects[0]
		if edited {
			original := repo.version.Manifest.Resources[0]
			current := original
			current.ID = "edited"
			current.Role = CurrentVideo
			current.Descriptor = &ResourceDescriptor{6, "edited.mov", "com.apple.quicktime-movie"}
			repo.version.Manifest.Resources = append(repo.version.Manifest.Resources, current)
			repo.version.Manifest.Edited = true
			expected.ResourceID = current.ID
			expected.Key = sealedKey(Upload{ID: repo.version.VersionID, OwnerID: repo.owner}, current.ID)
			repo.version.Objects = append(repo.version.Objects, expected)
			repo.version.OriginalBytes += current.SizeBytes
			repo.version.ManifestDigest, _ = repo.version.Manifest.Digest()
		}
		if _, err := service.PhotoCandidate(context.Background(), repo.owner, repo.version.VersionID, PhotoGridJPEG); err != nil || renderer.calls != 1 || renderer.selected != expected {
			t.Fatal("wrong displayed video", err)
		}
		for _, preset := range []PhotoPreset{PhotoGridPNG, PhotoDailyJPEG, PhotoDailyPNG} {
			if _, err := service.PhotoCandidate(context.Background(), repo.owner, repo.version.VersionID, preset); err == nil || renderer.calls != 1 {
				t.Fatal("video became daily photo", preset, err)
			}
		}
	}
}

type coverReadRepository struct{ *previewReadRepository }

func (r *coverReadRepository) PhotoWrite(_ context.Context, owner, version string, preset PhotoPreset) (PhotoWriteRecord, error) {
	if owner != r.owner || version != r.version.VersionID || preset != PhotoGridJPEG {
		return PhotoWriteRecord{}, ErrNotFound
	}
	return r.record, nil
}
func TestVideoCoverReadRequiresOwnedVerifiedReceiptAndNeverGrantsReplacement(t *testing.T) {
	service, repo, signer := previewReadFixture(t)
	repo.version.Manifest.Kind = Video
	repo.version.Manifest.Metadata = &AssetMetadata{PixelWidth: 1920, PixelHeight: 1080, DurationMilliseconds: 1000}
	repo.version.Manifest.Resources[0].Role = OriginalVideo
	repo.version.Manifest.Resources[0].Descriptor = &ResourceDescriptor{2, "source.mov", "com.apple.quicktime-movie"}
	repo.version.ManifestDigest, _ = repo.version.Manifest.Digest()
	repo.record.ManifestDigest = repo.version.ManifestDigest
	repo.record.Spec.Preset = PhotoGridJPEG
	repo.record.Spec.Width = 512
	repo.record.Spec.Height = 288
	service.repository = &coverReadRepository{repo}
	grant, err := service.PhotoPreviewReadGrant(context.Background(), repo.owner, repo.version.VersionID, PhotoGridJPEG)
	if err != nil || grant.ReplacementEligible || signer.calls != 1 || grant.Preset != PhotoGridJPEG {
		t.Fatal("cover read", err)
	}
	for _, preset := range []PhotoPreset{PhotoDailyJPEG, PhotoDailyPNG, PhotoGridPNG} {
		if _, err = service.PhotoPreviewReadGrant(context.Background(), repo.owner, repo.version.VersionID, preset); !errors.Is(err, ErrNotFound) || signer.calls != 1 {
			t.Fatal("noncover signed", err)
		}
	}
	if _, err = service.PhotoPreviewReadGrant(context.Background(), uuid.NewString(), repo.version.VersionID, PhotoGridJPEG); err == nil || signer.calls != 1 {
		t.Fatal("cross account cover", err)
	}
	repo.record.ObjectVersion = ""
	if _, err = service.PhotoPreviewReadGrant(context.Background(), repo.owner, repo.version.VersionID, PhotoGridJPEG); err == nil || signer.calls != 1 {
		t.Fatal("unverified cover", err)
	}
}

func TestVideoCoverBudgetAndPresetRemainBounded(t *testing.T) {
	service, repo, _ := videoReadFixture(t)
	service.repository = &uploadTestRepository{values: map[string]Upload{}}
	service.policy.MaxOriginalBytes = 1 << 30
	service.policy.MaxDerivedBytes = 64 << 20
	small, err := service.Create(context.Background(), repo.owner, uuid.NewString(), repo.version.Manifest)
	if err != nil || small.DerivedBudget != small.OriginalBytes+(2<<20) {
		t.Fatal("cover not reserved", small.DerivedBudget, err)
	}
	for _, maximum := range []int64{1, 100, 1 << 20, 64 << 20} {
		if v := videoCoverBudget(maximum); v < 0 || v > maximum/2 || v > 2<<20 {
			t.Fatal("unbounded cover reservation")
		}
	}
	if p := photoJobPresets(repo.version.Manifest); len(p) != 1 || p[0] != PhotoGridJPEG {
		t.Fatal("wrong cover jobs", p)
	}
	repo.version.Manifest.Metadata.MediaSubtypes = 1
	if len(photoJobPresets(repo.version.Manifest)) != 0 {
		t.Fatal("special subtype processed")
	}
}

func TestMySQLVideoCoverJobsQuotaFinalizationAndCleanupInventory(t *testing.T) {
	db := photoJournalDB(t)
	u, _ := videoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM album_photo_jobs WHERE owner_id=?`, u.OwnerID)
		db.Exec(`DELETE FROM album_photo_writes WHERE owner_id=?`, u.OwnerID)
	})
	for range 2 {
		if _, err := r.Complete(ctx, u.OwnerID, u.ID, "", nil, now); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewUploadService(r, &objectTestStore{}, UploadPolicy{1 << 30, 64 << 20, time.Hour, time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	small := u.Manifest
	small.Resources = append([]Resource(nil), u.Manifest.Resources...)
	small.Resources[0].SizeBytes = 1000
	reserved, err := service.Create(ctx, u.OwnerID, uuid.NewString(), small)
	if err != nil || reserved.DerivedBudget != reserved.OriginalBytes+(2<<20) {
		t.Fatal("database did not reserve the cover", reserved.DerivedBudget, err)
	}
	jobs := ownPhotoJobs(t, r, u.OwnerID)
	if len(jobs) != 1 || jobs[0].Preset != PhotoGridJPEG {
		t.Fatal("cover queue not idempotent", jobs)
	}
	if _, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now); !errors.Is(err, ErrDerivedPending) {
		t.Fatal("budget closed before cover", err)
	}
	job, err := r.ClaimPhotoJob(ctx, u.OwnerID, jobs[0].ID, uuid.NewString(), now)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := inspectPhotoCandidate(candidateImage(t, "jpeg", 11), PhotoGridJPEG)
	if err != nil {
		t.Fatal(err)
	}
	spec := PhotoWriteSpec{OwnerID: u.OwnerID, AttemptID: uuid.NewString(), Preset: PhotoGridJPEG, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height}
	record, fresh, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, now)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	key, _ := spec.key()
	receipt := SealedObject{ResourceID: string(spec.Preset), Key: key, VersionID: "cover-v1", SizeBytes: spec.SizeBytes, SHA256: spec.SHA256}
	if _, err = r.CompletePhotoWrite(ctx, record, receipt, now); err != nil {
		t.Fatal(err)
	}
	if err = r.FinishPhotoJob(ctx, job, true, false, now); err != nil {
		t.Fatal(err)
	}
	// A terminal video failure still permits retaining the useful cover and closing escrow.
	_, err = db.Exec(`UPDATE album_video_jobs SET state='attention' WHERE owner_id=?`, u.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	final, err := r.FinalizeDerivedBudget(ctx, DerivedVersionRef{u.OwnerID, u.ID}, now)
	if err != nil || final.RetainedBytes != spec.SizeBytes || final.ReleasedBytes != u.DerivedBudget-spec.SizeBytes {
		t.Fatal("cover settlement", final, err)
	}
	c := derivedCleanupFor(t, r, u.OwnerID, "photo", spec.AttemptID)
	provider := &cleanupProviderFixture{versions: []DerivedObjectVersion{{VersionID: receipt.VersionID, SizeBytes: receipt.SizeBytes}}, receipt: &receipt}
	worker := cleanupTestWorker(t, r, c, provider, &now)
	if _, err := worker.RunOnce(ctx); err != nil || len(provider.deletes) != 0 {
		t.Fatal("retained cover removed", err)
	}
	if !strings.Contains(key, u.OwnerID) {
		t.Fatal("cover escaped owner")
	}
}

func TestMySQLAccountDeletionObservesUncertainVideoCoverWithoutPublishingOrRefunding(t *testing.T) {
	f := newPushFixture(t)
	r := NewMySQLUploads(f.r.db)
	u, _ := videoJournalUpload(t, f.r.db)
	ctx := context.Background()
	candidate, err := inspectPhotoCandidate(candidateImage(t, "jpeg", 11), PhotoGridJPEG)
	if err != nil {
		t.Fatal(err)
	}
	spec := PhotoWriteSpec{OwnerID: u.OwnerID, AttemptID: uuid.NewString(), Preset: PhotoGridJPEG, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height}
	record, fresh, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, f.now)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	deleteProtocolFixtureOwner(t, f, u.OwnerID)
	beforeUsed, beforeReserved := photoJournalBalance(t, f.r.db, u.OwnerID)
	observed := 0
	batch, err := r.runPhotoDeletionBatch(ctx, func(_ context.Context, input PhotoWriteSpec) (string, error) {
		if input != spec {
			t.Fatal("wrong cover")
		}
		observed++
		return "exact-cover-version", nil
	}, 1, func() time.Time { return f.now })
	if err != nil || observed != 1 || batch.Acknowledged != 1 {
		t.Fatal("uncertain cover could not join account deletion", batch, err)
	}
	c := photoDeletionCheck{account: u.OwnerID, attempt: record.Spec.AttemptID, record: record}
	assertPhotoDeletionObservation(t, r, c, "exact-cover-version")
	p, err := r.PhotoWriteByAttempt(ctx, u.OwnerID, record.Spec.AttemptID)
	if err != nil || p.ObjectVersion != "" {
		t.Fatal("observation published cover", err)
	}
	used, reserved := photoJournalBalance(t, f.r.db, u.OwnerID)
	if used != beforeUsed || reserved != beforeReserved {
		t.Fatal("observation refunded cover")
	}
}
