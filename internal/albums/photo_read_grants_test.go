package albums

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

type previewReadRepository struct {
	*readRepository
	record PhotoWriteRecord
}

func (r *previewReadRepository) PhotoWrite(_ context.Context, owner, version string, preset PhotoPreset) (PhotoWriteRecord, error) {
	if owner != r.owner || version != r.version.VersionID || preset != PhotoDailyJPEG {
		return PhotoWriteRecord{}, ErrNotFound
	}
	return r.record, nil
}
func (*previewReadRepository) BeginPhotoWrite(context.Context, string, string, PhotoWriteSpec, time.Time) (PhotoWriteRecord, bool, error) {
	panic("read must not begin write")
}
func (*previewReadRepository) CompletePhotoWrite(context.Context, PhotoWriteRecord, SealedObject, time.Time) (PhotoWriteRecord, error) {
	panic("read must not complete write")
}

type previewReadSigner struct {
	*readSigner
	record PhotoWriteRecord
	expiry time.Time
}

func (s *previewReadSigner) AuthorizePhotoPreviewRead(_ context.Context, r PhotoWriteRecord, expiry time.Time) (string, error) {
	s.calls++
	s.record = r
	s.expiry = expiry
	return "https://storage.example/fixed-preview", nil
}
func previewReadFixture(t *testing.T) (*UploadService, *previewReadRepository, *previewReadSigner) {
	t.Helper()
	service, repo, signer := readFixture(t)
	record := PhotoWriteRecord{VersionID: repo.version.VersionID, ManifestDigest: repo.version.ManifestDigest, Spec: PhotoWriteSpec{OwnerID: repo.owner, AttemptID: uuid.NewString(), Preset: PhotoDailyJPEG, SizeBytes: 123, SHA256: strings.Repeat("a", 64), Width: 800, Height: 600}, ObjectVersion: "preview+/=version"}
	r := &previewReadRepository{repo, record}
	s := &previewReadSigner{readSigner: signer}
	service.repository = r
	service.objects = s
	return service, r, s
}
func TestPhotoPreviewReadRequiresCompletedOwnedSourceBoundRecord(t *testing.T) {
	s, r, signer := previewReadFixture(t)
	grant, err := s.PhotoPreviewReadGrant(context.Background(), r.owner, r.version.VersionID, PhotoDailyJPEG)
	if err != nil || signer.calls != 1 || signer.record != r.record || grant.ReplacementEligible || grant.ManifestDigest != r.version.ManifestDigest || grant.ObjectVersionID != r.record.ObjectVersion || grant.SHA256 != r.record.Spec.SHA256 || grant.Width != 800 || grant.Height != 600 || grant.SizeBytes != 123 || grant.ContentType != "image/jpeg" || !grant.ExpiresAt.Equal(s.now().Add(5*time.Minute)) {
		t.Fatal("wrong preview grant", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*previewReadRepository)
		want   error
	}{
		{"unsettled", func(r *previewReadRepository) { r.record.ObjectVersion = "" }, ErrPhotoWriteUncertain},
		{"null version", func(r *previewReadRepository) { r.record.ObjectVersion = "null" }, ErrPhotoWriteUncertain},
		{"cross owner", func(r *previewReadRepository) { r.record.Spec.OwnerID = uuid.NewString() }, ErrConflict},
		{"cross source", func(r *previewReadRepository) { r.record.VersionID = uuid.NewString() }, ErrConflict},
		{"changed manifest", func(r *previewReadRepository) { r.record.ManifestDigest = strings.Repeat("b", 64) }, ErrConflict},
		{"wrong preset", func(r *previewReadRepository) { r.record.Spec.Preset = PhotoGridJPEG }, ErrConflict},
		{"bad dimensions", func(r *previewReadRepository) { r.record.Spec.Width = 9000 }, ErrPhotoWriteUncertain},
		{"bad checksum", func(r *previewReadRepository) { r.record.Spec.SHA256 = "invalid" }, ErrPhotoWriteUncertain},
		{"partial original", func(r *previewReadRepository) { r.version.Objects = nil }, ErrIncompleteBackup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, signer := previewReadFixture(t)
			tc.mutate(r)
			_, err := s.PhotoPreviewReadGrant(context.Background(), r.owner, r.version.VersionID, PhotoDailyJPEG)
			if !errors.Is(err, tc.want) || signer.calls != 0 {
				t.Fatal("invalid evidence reached signer", err)
			}
		})
	}
	for _, tc := range []struct {
		owner, version string
		preset         PhotoPreset
	}{{uuid.NewString(), r.version.VersionID, PhotoDailyJPEG}, {r.owner, uuid.NewString(), PhotoDailyJPEG}, {r.owner, r.version.VersionID, PhotoGridJPEG}, {r.owner, r.version.VersionID, "arbitrary-rule"}} {
		if _, err := s.PhotoPreviewReadGrant(context.Background(), tc.owner, tc.version, tc.preset); err == nil {
			t.Fatal("unauthorized preview accepted")
		}
	}
	if signer.calls != 1 {
		t.Fatal("unauthorized request signed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.PhotoPreviewReadGrant(ctx, r.owner, r.version.VersionID, PhotoDailyJPEG); !errors.Is(err, context.Canceled) || signer.calls != 1 {
		t.Fatal("cancelled read signed", err)
	}
}
func TestPhotoPreviewReadHTTPContractAndNoSideEffects(t *testing.T) {
	s, r, signer := previewReadFixture(t)
	router, err := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", r.owner}}, s)
	if err != nil {
		t.Fatal(err)
	}
	path := "https://albums.example/v1/albums/library/versions/" + r.version.VersionID + "/photo-previews/daily_jpeg_v1/read-grant"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", path, nil))
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(response.Code)
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
	if err := doc.Components.Schemas["PhotoPreviewReadGrant"].Value.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
	value.(map[string]any)["replacementEligible"] = true
	if err := doc.Components.Schemas["PhotoPreviewReadGrant"].Value.VisitJSON(value); err == nil {
		t.Fatal("contract permits replacement proof")
	}
	for _, request := range []*http.Request{
		httptest.NewRequest("POST", path+"?owner=other", nil), httptest.NewRequest("POST", path, strings.NewReader("{}")),
		httptest.NewRequest("GET", path, nil), httptest.NewRequest("POST", strings.Replace(path, "daily_jpeg_v1", "arbitrary", 1), nil),
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
		t.Fatal("bad request signed")
	}
	r.record.ObjectVersion = ""
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", path, nil))
	if response.Code != 409 || !strings.Contains(response.Body.String(), "photo_preview_not_ready") || signer.calls != 1 {
		t.Fatal("unsettled read not rejected", response.Code)
	}
}
func TestCOSPhotoPreviewGrantPinsExactVersionAndOnlyGET(t *testing.T) {
	_, repo, _ := previewReadFixture(t)
	record := repo.record
	s := cosFixture(t, func(*http.Request) (*http.Response, error) { t.Fatal("read grant contacted provider"); return nil, nil })
	now := time.Unix(1790000000, 0)
	s.now = func() time.Time { return now }
	signed, err := s.AuthorizePhotoPreviewRead(context.Background(), record, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	key, _ := record.Spec.key()
	if u.Scheme != "https" || u.Host != s.host || u.Path != "/"+key || q.Get("versionId") != record.ObjectVersion || q.Get("q-url-param-list") != "versionid" || q.Get("q-header-list") != "host" {
		t.Fatal("unbound preview URL")
	}
	authTime := &cos.AuthTime{SignStartTime: now, SignEndTime: now.Add(5 * time.Minute), KeyStartTime: now, KeyEndTime: now.Add(5 * time.Minute)}
	signature := func(method, path, version string) string {
		target := url.URL{Scheme: "https", Host: s.host, Path: path, RawQuery: url.Values{"versionId": []string{version}}.Encode()}
		req, _ := http.NewRequest(method, target.String(), nil)
		cos.AddAuthorizationHeader("test-id", "test-key", "", req, authTime)
		values, _ := url.ParseQuery(req.Header.Get("Authorization"))
		return values.Get("q-signature")
	}
	if signature("GET", u.Path, record.ObjectVersion) != q.Get("q-signature") {
		t.Fatal("signature mismatch")
	}
	if signature("PUT", u.Path, record.ObjectVersion) == q.Get("q-signature") || signature("GET", u.Path, "other") == q.Get("q-signature") || signature("GET", u.Path+"other", record.ObjectVersion) == q.Get("q-signature") {
		t.Fatal("grant can change operation")
	}
	for _, duration := range []time.Duration{0, -time.Second, 5*time.Minute + time.Second} {
		if _, err := s.AuthorizePhotoPreviewRead(context.Background(), record, now.Add(duration)); err == nil {
			t.Fatal("bad expiry signed")
		}
	}
	for _, version := range []string{"", "null", "bad\nversion", strings.Repeat("x", 1025)} {
		bad := record
		bad.ObjectVersion = version
		if _, err := s.AuthorizePhotoPreviewRead(context.Background(), bad, now.Add(time.Minute)); err == nil {
			t.Fatal("bad provider version signed")
		}
	}
	bad := record
	bad.Spec.OwnerID = "../originals"
	if _, err := s.AuthorizePhotoPreviewRead(context.Background(), bad, now.Add(time.Minute)); err == nil {
		t.Fatal("namespace escape signed")
	}
}

func TestPhotoPreviewLiveStillNeverAuthorizesReplacementAndVideoIsRejected(t *testing.T) {
	for _, kind := range []AssetKind{LivePhoto, Video} {
		t.Run(string(kind), func(t *testing.T) {
			s, r, signer := previewReadFixture(t)
			r.version.Manifest.Kind = kind
			if kind == LivePhoto {
				paired := Resource{ID: "paired", Role: PairedVideo, SizeBytes: 17, SHA256: strings.Repeat("b", 64)}
				r.version.Manifest.Resources = append(r.version.Manifest.Resources, paired)
				r.version.Objects = append(r.version.Objects, SealedObject{ResourceID: paired.ID, Key: sealedKey(Upload{ID: r.version.VersionID, OwnerID: r.owner}, paired.ID), VersionID: "paired-version", SizeBytes: 17, SHA256: paired.SHA256})
				r.version.OriginalBytes += 17
			} else {
				r.version.Manifest.Resources[0].Role = OriginalVideo
				r.version.Manifest.Resources[0].Descriptor = nil
			}
			var err error
			r.version.ManifestDigest, err = r.version.Manifest.Digest()
			if err != nil {
				t.Fatal(err)
			}
			r.record.ManifestDigest = r.version.ManifestDigest
			grant, err := s.PhotoPreviewReadGrant(context.Background(), r.owner, r.version.VersionID, PhotoDailyJPEG)
			if kind == LivePhoto {
				if err != nil || grant.ReplacementEligible || signer.calls != 1 {
					t.Fatal("Live still replacement boundary failed", err)
				}
			} else {
				if !errors.Is(err, ErrNotFound) || signer.calls != 0 {
					t.Fatal("video signed as still", err)
				}
			}
		})
	}
}
