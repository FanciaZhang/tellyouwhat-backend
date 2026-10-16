package albums

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

type videoReadRepository struct {
	*readRepository
	job VideoJob
}

func (r *videoReadRepository) VideoJob(_ context.Context, owner, version string, preset VideoPreset) (VideoJob, error) {
	if owner != r.owner || version != r.version.VersionID || preset != VideoDaily1080 {
		return VideoJob{}, ErrNotFound
	}
	return r.job, nil
}
func (*videoReadRepository) BeginVideoSubmission(context.Context, VideoJob, VideoTranscodeSpec, time.Time) (VideoJob, bool, error) {
	panic("a read must not submit")
}
func (*videoReadRepository) AttachVideoSubmission(context.Context, VideoJob, string, time.Time) (VideoJob, error) {
	panic("a read must not attach")
}
func (*videoReadRepository) CompleteVideoJob(context.Context, VideoJob, VideoResult, time.Time) (VideoJob, error) {
	panic("a read must not complete")
}

type videoReadSigner struct{ *readSigner }

func (s *videoReadSigner) AuthorizeVideoPreviewRead(context.Context, VideoJob, time.Time) (string, error) {
	s.calls++
	return "https://storage.example/fixed-video", nil
}

func videoReadFixture(t *testing.T) (*UploadService, *videoReadRepository, *videoReadSigner) {
	t.Helper()
	s, r, signer := readFixture(t)
	v := &r.version
	v.Manifest.Kind = Video
	v.Manifest.Metadata = &AssetMetadata{PixelWidth: 1920, PixelHeight: 1080, DurationMilliseconds: 1000}
	v.Manifest.Resources[0].Role = OriginalVideo
	v.Manifest.Resources[0].Descriptor = &ResourceDescriptor{2, "source.MOV", "com.apple.quicktime-movie"}
	v.ManifestDigest, _ = v.Manifest.Digest()
	j := videoJobFixture()
	j.OwnerID, j.VersionID, j.ManifestDigest, j.State = r.owner, v.VersionID, v.ManifestDigest, "completed"
	j.Spec.Source = v.Objects[0]
	j.Result = new(VideoResult)
	*j.Result = journalVideoResult(j)
	repo := &videoReadRepository{r, j}
	object := &videoReadSigner{signer}
	s.repository, s.objects = repo, object
	return s, repo, object
}

func TestVideoPreviewReadRequiresCompleteOwnedSourceBoundReceipt(t *testing.T) {
	s, r, signer := videoReadFixture(t)
	grant, err := s.VideoPreviewReadGrant(context.Background(), r.owner, r.version.VersionID, VideoDaily1080)
	if err != nil || signer.calls != 1 || grant.ReplacementEligible || grant.ManifestDigest != r.version.ManifestDigest || grant.ObjectVersionID != r.job.Result.Object.VersionID ||
		grant.SHA256 != r.job.Result.Object.SHA256 || grant.ContentType != "video/mp4" || grant.Profile.FrameRate != 60 || !grant.ExpiresAt.Equal(s.now().Add(5*time.Minute)) {
		t.Fatal("wrong owned video grant", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*videoReadRepository)
	}{
		{"pending", func(r *videoReadRepository) { r.job.State = "queued"; r.job.Result = nil }},
		{"owner", func(r *videoReadRepository) { r.job.OwnerID = uuid.NewString() }},
		{"manifest", func(r *videoReadRepository) { r.job.ManifestDigest = strings.Repeat("f", 64) }},
		{"source", func(r *videoReadRepository) { r.job.Spec.Source.VersionID = "changed" }},
		{"result-version", func(r *videoReadRepository) { r.job.Result.Object.VersionID = "null" }},
		{"result-hash", func(r *videoReadRepository) { r.job.Result.Object.SHA256 = "invalid" }},
		{"result-fps", func(r *videoReadRepository) { r.job.Result.Profile.FrameRate = 30 }},
		{"partial-backup", func(r *videoReadRepository) { r.version.Objects = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, signer := videoReadFixture(t)
			tc.edit(r)
			if _, err := s.VideoPreviewReadGrant(context.Background(), r.owner, r.version.VersionID, VideoDaily1080); err == nil || signer.calls != 0 {
				t.Fatal("invalid receipt reached signer", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.VideoPreviewReadGrant(ctx, r.owner, r.version.VersionID, VideoDaily1080); !errors.Is(err, context.Canceled) || signer.calls != 1 {
		t.Fatal("cancelled request signed", err)
	}
}

func TestVideoPreviewHTTPContractIsBodylessScopedAndHasNoProcessingSideEffects(t *testing.T) {
	s, r, signer := videoReadFixture(t)
	router, err := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", r.owner}}, s)
	if err != nil {
		t.Fatal(err)
	}
	path := "https://albums.example/v1/albums/library/versions/" + r.version.VersionID + "/video-previews/daily_mp4_1080_v1/read-grant"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", path, nil))
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("grant not served privately", response.Code)
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var value any
	if json.Unmarshal(response.Body.Bytes(), &value) != nil {
		t.Fatal("bad JSON")
	}
	if err := doc.Components.Schemas["VideoPreviewReadGrant"].Value.VisitJSON(value); err != nil {
		t.Fatal("response violates contract", err)
	}
	value.(map[string]any)["replacementEligible"] = true
	if err := doc.Components.Schemas["VideoPreviewReadGrant"].Value.VisitJSON(value); err == nil {
		t.Fatal("contract permits deletion authority")
	}
	for _, request := range []*http.Request{
		httptest.NewRequest("POST", path+"?owner=other", nil), httptest.NewRequest("POST", path, strings.NewReader("{}")),
		httptest.NewRequest("GET", path, nil), httptest.NewRequest("POST", strings.Replace(path, "daily_mp4_1080_v1", "arbitrary", 1), nil),
		httptest.NewRequest("POST", strings.Replace(path, r.version.VersionID, uuid.NewString(), 1), nil),
		httptest.NewRequest("POST", strings.Replace(path, "albums.example", "other.example", 1), nil),
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code < 400 {
			t.Fatal("bad request accepted", response.Code)
		}
	}
	if signer.calls != 1 {
		t.Fatal("invalid request signed")
	}
	r.job.State, r.job.Result = "queued", nil
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", path, nil))
	if response.Code != 409 || !strings.Contains(response.Body.String(), "video_preview_not_ready") || signer.calls != 1 {
		t.Fatal("pending job fabricated playable result", response.Code)
	}
}
