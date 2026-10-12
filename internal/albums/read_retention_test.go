package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type retainedReadRepository struct {
	UploadRepository
	LibraryRepository
	PhotoWriteRepository
	VideoJobRepository
	LivePairRepository
	pair      LivePairUpload
	authorize func(context.Context, string, AssetVersion, time.Time, time.Time, func(time.Time) error) error
}

func (r *retainedReadRepository) WithCloudReadGrant(ctx context.Context, owner string, v AssetVersion, now, expiry time.Time, sign func(time.Time) error) error {
	return r.authorize(ctx, owner, v, now, expiry, sign)
}
func (r *retainedReadRepository) LivePair(_ context.Context, owner, version string) (LivePairUpload, error) {
	if owner != r.pair.OwnerID || version != r.pair.VersionID {
		return LivePairUpload{}, ErrNotFound
	}
	return r.pair, nil
}

type retainedReadSigner struct {
	AlbumObjects
	LivePairStorage
	expiries []time.Time
}

func (s *retainedReadSigner) read(expiry time.Time) (string, error) {
	s.expiries = append(s.expiries, expiry)
	return "https://storage.example/fixed-retained-object", nil
}
func (s *retainedReadSigner) AuthorizeOriginalRead(_ context.Context, _ SealedObject, expiry time.Time) (string, error) {
	return s.read(expiry)
}
func (s *retainedReadSigner) AuthorizePhotoPreviewRead(_ context.Context, _ PhotoWriteRecord, expiry time.Time) (string, error) {
	return s.read(expiry)
}
func (s *retainedReadSigner) AuthorizeVideoPreviewRead(_ context.Context, _ VideoJob, expiry time.Time) (string, error) {
	return s.read(expiry)
}
func (s *retainedReadSigner) AuthorizeLivePairRead(_ context.Context, _ LivePairUpload, _ SealedObject, expiry time.Time) (string, error) {
	return s.read(expiry)
}

func TestCloudRetentionBoundsEveryDirectMediaRead(t *testing.T) {
	for _, kind := range []string{"original", "photo", "video", "live-components", "live-pair"} {
		for _, denied := range []bool{false, true} {
			name := kind
			if denied {
				name += "-expired"
			}
			t.Run(name, func(t *testing.T) {
				var s *UploadService
				var owner string
				var v AssetVersion
				r := &retainedReadRepository{}
				switch kind {
				case "original":
					service, repo, _ := readFixture(t)
					s, owner, v = service, repo.owner, repo.version
					r.LibraryRepository = repo
				case "photo":
					service, repo, _ := previewReadFixture(t)
					s, owner, v = service, repo.owner, repo.version
					r.LibraryRepository = repo
					r.PhotoWriteRepository = repo
				case "video":
					service, repo, _ := videoReadFixture(t)
					s, owner, v = service, repo.owner, repo.version
					r.LibraryRepository = repo
					r.VideoJobRepository = repo
				default:
					service, repo, _ := liveReadFixture(t)
					s, owner, v = service, repo.owner, repo.version
					r.LibraryRepository = repo
					r.PhotoWriteRepository = repo
					r.VideoJobRepository = repo
					r.pair = LivePairUpload{ID: uuid.NewString(), OwnerID: owner, VersionID: v.VersionID, AssetID: v.AssetID, ManifestDigest: v.ManifestDigest, Preset: DailyLivePair, State: "verified", Descriptor: pairDescriptor(repo.record, repo.job)}
					for _, id := range []string{"photo", "motion"} {
						resource, _ := r.pair.resource(id)
						key, _ := r.pair.key(id, false)
						r.pair.Objects = append(r.pair.Objects, SealedObject{id, key, "immutable-pair-version", resource.SizeBytes, resource.SHA256})
					}
				}
				r.UploadRepository = s.repository
				signer := &retainedReadSigner{AlbumObjects: s.objects}
				s.repository, s.objects = r, signer
				deadline := s.now().Add(time.Second)
				checks := 0
				r.authorize = func(_ context.Context, gotOwner string, got AssetVersion, _ time.Time, _ time.Time, sign func(time.Time) error) error {
					checks++
					if gotOwner != owner || got.VersionID != v.VersionID || got.ManifestDigest != v.ManifestDigest {
						t.Error("authorization scope changed")
					}
					if denied {
						return ErrNotFound
					}
					return sign(deadline)
				}
				var expiry time.Time
				var err error
				switch kind {
				case "original":
					g, e := s.OriginalReadGrant(context.Background(), owner, v.VersionID, "photo")
					expiry, err = g.ExpiresAt, e
				case "photo":
					g, e := s.PhotoPreviewReadGrant(context.Background(), owner, v.VersionID, PhotoDailyJPEG)
					expiry, err = g.ExpiresAt, e
				case "video":
					g, e := s.VideoPreviewReadGrant(context.Background(), owner, v.VersionID, VideoDaily1080)
					expiry, err = g.ExpiresAt, e
				case "live-components":
					g, e := s.LivePhotoPreviewReadGrant(context.Background(), owner, v.VersionID)
					expiry, err = g.ExpiresAt, e
				case "live-pair":
					g, e := s.LivePairReadGrant(context.Background(), owner, v.VersionID)
					expiry, err = g.ExpiresAt, e
				}
				if denied {
					if !errors.Is(err, ErrNotFound) || len(signer.expiries) != 0 || checks != 1 {
						t.Fatal("expired read reached signer", err, checks, len(signer.expiries))
					}
					return
				}
				count := 1
				if kind == "live-components" || kind == "live-pair" {
					count = 2
				}
				if err != nil || !expiry.Equal(deadline) || len(signer.expiries) != count || checks == 0 {
					t.Fatal("retention not enforced", expiry, err, checks, len(signer.expiries))
				}
				for _, expiry := range signer.expiries {
					if !expiry.Equal(deadline) {
						t.Fatal("actual signed URL outlives retention")
					}
				}
			})
		}
	}
}
func TestCloudReadSigningRejectsDeadlineCrossedWhileWaiting(t *testing.T) {
	s, repo, _ := readFixture(t)
	clock := s.now()
	s.now = func() time.Time { return clock }
	for _, phase := range []string{"before", "during"} {
		t.Run(phase, func(t *testing.T) {
			deadline := clock.Add(time.Second)
			calls := 0
			r := &retainedReadRepository{UploadRepository: repo, LibraryRepository: repo}
			r.authorize = func(_ context.Context, _ string, _ AssetVersion, _, _ time.Time, sign func(time.Time) error) error {
				if phase == "before" {
					clock = deadline
				}
				return sign(deadline)
			}
			s.repository = r
			_, err := s.authorizeLibraryRead(context.Background(), repo.owner, repo.version, func(time.Time) error { calls++; clock = deadline; return nil })
			want := 1
			if phase == "before" {
				want = 0
			}
			if !errors.Is(err, ErrNotFound) || calls != want {
				t.Fatal("expired result escaped", calls, err)
			}
		})
	}
}
