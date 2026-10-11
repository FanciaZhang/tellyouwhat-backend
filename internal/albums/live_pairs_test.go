package albums

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func pairBytes(data string) LivePairResource {
	h := sha256.Sum256([]byte(data))
	return LivePairResource{int64(len(data)), hex.EncodeToString(h[:])}
}
func pairDescriptor(p PhotoWriteRecord, j VideoJob) LivePairDescriptor {
	return LivePairDescriptor{SourcePhoto: LivePairPhotoSource{string(PhotoDailyJPEG), p.Spec.SizeBytes, p.Spec.SHA256, p.Spec.Width, p.Spec.Height, "image/jpeg"},
		SourceVideo: LivePairVideoSource{string(VideoDaily1080), LivePairResource{j.Result.Object.SizeBytes, j.Result.Object.SHA256}, j.Result.Profile},
		Timing:      *j.Spec.LivePhotoTiming, Photo: pairBytes("paired-jpeg"), Movie: pairBytes("paired-quicktime"),
		Pair: LivePairInfo{uuid.NewString(), .5, 1, p.Spec.Width, p.Spec.Height, true, false}}
}
func pairUploadFixture(t *testing.T) LivePairUpload {
	_, r, _ := liveReadFixture(t)
	p, _ := r.record.Receipt()
	return LivePairUpload{ID: uuid.NewString(), OwnerID: r.owner, RequestID: uuid.NewString(), AssetID: r.version.AssetID, VersionID: r.version.VersionID,
		ManifestDigest: r.version.ManifestDigest, Preset: DailyLivePair, Descriptor: pairDescriptor(r.record, r.job), SourcePhoto: p, SourceVideo: r.job.Result.Object,
		State: "uploading", ExpiresAt: time.Now().Add(time.Hour)}
}
func TestLivePairDeclarationsRejectInvalidTimelineAndUnboundedOrMismatchedProfiles(t *testing.T) {
	u := pairUploadFixture(t)
	if err := u.Descriptor.validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*LivePairDescriptor){
		func(d *LivePairDescriptor) { d.Pair.Identifier = strings.ToUpper(d.Pair.Identifier) },
		func(d *LivePairDescriptor) { d.Pair.PhotoTime = 1 }, func(d *LivePairDescriptor) { d.Pair.Duration = .4 },
		func(d *LivePairDescriptor) { d.Timing.PhotoResourceID = d.Timing.VideoResourceID },
		func(d *LivePairDescriptor) { d.SourceVideo.Width = 8192 }, func(d *LivePairDescriptor) { d.Pair.HasHDRVideo = true },
		func(d *LivePairDescriptor) { d.Pair.HasAudio = false }, func(d *LivePairDescriptor) { d.Photo.SizeBytes = 16<<20 + 1 },
		func(d *LivePairDescriptor) { d.Movie.SizeBytes = 4<<30 + 1 }, func(d *LivePairDescriptor) { d.Movie.SHA256 = "null" },
		func(d *LivePairDescriptor) { d.Pair.PhotoWidth++ },
	} {
		copy := u.Descriptor
		change(&copy)
		if copy.validate() == nil {
			t.Fatal("invalid declaration accepted")
		}
	}
}
func TestCOSLivePairAdapterSeparatesStagingAndArchiveAndPinsEveryRead(t *testing.T) {
	u := pairUploadFixture(t)
	now := time.Now()
	u.ExpiresAt = now.Add(time.Hour)
	copies, heads, reads := 0, 0, 0
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case http.MethodHead:
			heads++
			response := cosResponse(r, "", "staging-v1")
			response.ContentLength = u.Descriptor.Photo.SizeBytes
			return response, nil
		case http.MethodPut:
			copies++
			k, _ := u.key("photo", false)
			if r.URL.Path != "/"+k || !strings.HasSuffix(r.Header.Get("x-cos-copy-source"), "?versionId=staging-v1") {
				t.Error("unpinned or cross-scope COPY")
			}
			return cosResponse(r, `<CopyObjectResult><ETag>pair</ETag></CopyObjectResult>`, "pair+v2&query=#?"), nil
		case http.MethodGet:
			reads++
			if r.URL.Query().Get("versionId") != "pair+v2&query=#?" {
				t.Error("not fixed-version GET")
			}
			return cosResponse(r, "paired-jpeg", "pair+v2&query=#?"), nil
		default:
			t.Fatal("unexpected method", r.Method)
			return nil, nil
		}
	})
	s.now = func() time.Time { return now }
	g, err := s.AuthorizeLivePairPut(context.Background(), u, "photo", now.Add(5*time.Minute))
	key, _ := u.key("photo", true)
	if err != nil || !strings.Contains(g.URL, key) || g.Headers["Content-Type"] != "image/jpeg" || !strings.Contains(g.URL, "upload-test-id") {
		t.Fatal("bad staging grant", err)
	}
	if _, err := s.AuthorizePut(context.Background(), key, Resource{ID: "photo", SizeBytes: 10}, now.Add(time.Minute)); err == nil {
		t.Fatal("original signer widened")
	}
	u.State, u.PhotoConsumed = "verifying", true
	version, err := s.SealLivePairOnce(context.Background(), u, "photo")
	if err != nil || copies != 1 || heads != 1 {
		t.Fatal("seal", err)
	}
	body, err := s.OpenLivePairVersion(context.Background(), u, "photo", version)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "paired-jpeg" || reads != 1 {
		t.Fatal("wrong exact bytes")
	}
	k, _ := u.key("photo", false)
	object := SealedObject{"photo", k, version, int64(len(data)), u.Descriptor.Photo.SHA256}
	u.State = "verified"
	url, err := s.AuthorizeLivePairRead(context.Background(), u, object, now.Add(time.Minute))
	if err != nil || !strings.Contains(url, "test-id") || strings.Contains(url, "upload-test-id") {
		t.Fatal("bad archive signer", err)
	}
	if _, err := s.AuthorizeOriginalRead(context.Background(), object, now.Add(time.Minute)); err == nil {
		t.Fatal("original read authority widened")
	}
	object.Key = "albums/live-pairs/" + uuid.NewString() + "/" + u.ID + "/photo"
	if _, err := s.AuthorizeLivePairRead(context.Background(), u, object, now.Add(time.Minute)); err == nil {
		t.Fatal("cross account signed")
	}
}
func TestCOSLivePairOpenRejectsChangedVersionPartialAndEncodedResponses(t *testing.T) {
	u := pairUploadFixture(t)
	for _, change := range []func(*http.Response){
		func(r *http.Response) { r.Header.Set("x-cos-version-id", "different") }, func(r *http.Response) { r.ContentLength++ },
		func(r *http.Response) { r.Header.Set("Content-Range", "bytes 0-10/20") }, func(r *http.Response) { r.Header.Set("Content-Encoding", "gzip") },
		func(r *http.Response) { r.Uncompressed = true },
	} {
		s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
			response := cosResponse(r, "paired-jpeg", "pair-v1")
			change(response)
			return response, nil
		})
		if _, err := s.OpenLivePairVersion(context.Background(), u, "photo", "pair-v1"); !errors.Is(err, ErrIncompleteBackup) {
			t.Fatal("invalid response accepted", err)
		}
	}
}
