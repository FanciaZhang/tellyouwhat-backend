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

type readRepository struct {
	*uploadTestRepository
	owner   string
	version AssetVersion
}

func (r *readRepository) AssetVersion(_ context.Context, owner, id string) (AssetVersion, error) {
	if owner != r.owner || id != r.version.VersionID {
		return AssetVersion{}, ErrNotFound
	}
	return r.version, nil
}
func (r *readRepository) AssetVersions(context.Context, string, int64, int) (AssetVersionPage, error) {
	return AssetVersionPage{}, nil
}

type readSigner struct {
	*objectTestStore
	calls    int
	selected SealedObject
}

func (s *readSigner) AuthorizeOriginalRead(_ context.Context, object SealedObject, _ time.Time) (string, error) {
	s.calls++
	s.selected = object
	return "https://storage.example/signed-original", nil
}
func readFixture(t *testing.T) (*UploadService, *readRepository, *readSigner) {
	t.Helper()
	service, repo, objects, owner, manifest := uploadFixture(t)
	id := uuid.NewString()
	digest, _ := manifest.Digest()
	resource := manifest.Resources[0]
	version := AssetVersion{Sequence: 1, AssetID: manifest.AssetID, VersionID: id, ManifestDigest: digest, Manifest: manifest,
		OriginalBytes: resource.SizeBytes, PublishedAt: service.now(), Objects: []SealedObject{{ResourceID: resource.ID, Key: sealedKey(Upload{ID: id, OwnerID: owner}, resource.ID), VersionID: "immutable-v1", SizeBytes: resource.SizeBytes, SHA256: resource.SHA256}}}
	r := &readRepository{repo, owner, version}
	signer := &readSigner{objectTestStore: objects}
	service.repository = r
	service.objects = signer
	return service, r, signer
}
func TestOriginalReadGrantRequiresOwnedCompleteImmutableVersion(t *testing.T) {
	service, repo, signer := readFixture(t)
	ctx := context.Background()
	grant, err := service.OriginalReadGrant(ctx, repo.owner, repo.version.VersionID, "photo")
	if err != nil || signer.calls != 1 || signer.selected != repo.version.Objects[0] || grant.ManifestDigest != repo.version.ManifestDigest ||
		grant.ObjectVersionID != "immutable-v1" || grant.SizeBytes != repo.version.OriginalBytes || !grant.ExpiresAt.Equal(service.now().Add(5*time.Minute)) {
		t.Fatalf("wrong read grant: %v", err)
	}
	for _, args := range [][3]string{{uuid.NewString(), repo.version.VersionID, "photo"}, {repo.owner, uuid.NewString(), "photo"}, {repo.owner, repo.version.VersionID, "missing"}} {
		if _, err := service.OriginalReadGrant(ctx, args[0], args[1], args[2]); !errors.Is(err, ErrNotFound) {
			t.Fatal("unowned or unpublished read accepted", err)
		}
	}
	if signer.calls != 1 {
		t.Fatal("unauthorized request reached signer")
	}
	for _, mutate := range []func(*AssetVersion){
		func(v *AssetVersion) {
			v.Objects[0].Key = "albums/originals/" + uuid.NewString() + "/" + v.VersionID + "/photo"
		},
		func(v *AssetVersion) { v.Objects[0].VersionID = "null" },
		func(v *AssetVersion) { v.Objects[0].SHA256 = strings.Repeat("b", 64) },
		func(v *AssetVersion) { v.Objects = nil },
		func(v *AssetVersion) { v.ManifestDigest = strings.Repeat("b", 64) },
		func(v *AssetVersion) { v.OriginalBytes++ },
	} {
		service, r, signer := readFixture(t)
		mutate(&r.version)
		if _, err := service.OriginalReadGrant(ctx, r.owner, r.version.VersionID, "photo"); !errors.Is(err, ErrIncompleteBackup) || signer.calls != 0 {
			t.Fatal("unverified proof reached signer", err)
		}
	}
}
func TestOriginalReadHTTPIsBodylessScopedAndContractValid(t *testing.T) {
	service, repo, signer := readFixture(t)
	router, err := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", repo.owner}}, service)
	if err != nil {
		t.Fatal(err)
	}
	path := "https://albums.example/v1/albums/library/versions/" + repo.version.VersionID + "/resources/photo/read-grant"
	result := httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest("POST", path, nil))
	if result.Code != 200 || result.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(result.Code)
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(result.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if err := doc.Components.Schemas["OriginalReadGrant"].Value.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
	for _, request := range []*http.Request{
		httptest.NewRequest("POST", path+"?owner=other", nil), httptest.NewRequest("POST", path, strings.NewReader("{}")),
		httptest.NewRequest("GET", path, nil), httptest.NewRequest("POST", strings.Replace(path, repo.version.VersionID, uuid.NewString(), 1), nil),
	} {
		result := httptest.NewRecorder()
		router.ServeHTTP(result, request)
		if result.Code < 400 {
			t.Fatal("accepted invalid request", result.Code)
		}
	}
	if signer.calls != 1 {
		t.Fatal("invalid HTTP request reached signer")
	}
}
func TestCOSReadGrantPinsVersionHostVerbAndExpiry(t *testing.T) {
	s := cosFixture(t, func(*http.Request) (*http.Response, error) { t.Fatal("signing contacted storage"); return nil, nil })
	now := time.Unix(1790000000, 0)
	s.now = func() time.Time { return now }
	object := SealedObject{ResourceID: "photo", Key: "albums/originals/" + uuid.NewString() + "/" + uuid.NewString() + "/photo", VersionID: "version+/=opaque", SizeBytes: 23}
	signed, err := s.AuthorizeOriginalRead(context.Background(), object, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if parsed.Scheme != "https" || parsed.Host != s.host || parsed.Path != "/"+object.Key || q.Get("versionId") != object.VersionID || q.Get("q-url-param-list") != "versionid" || q.Get("q-header-list") != "host" {
		t.Fatal("unbound signed read")
	}
	authTime := &cos.AuthTime{SignStartTime: now, SignEndTime: now.Add(5 * time.Minute), KeyStartTime: now, KeyEndTime: now.Add(5 * time.Minute)}
	signature := func(method, path, version string) string {
		u := url.URL{Scheme: "https", Host: s.host, Path: path, RawQuery: url.Values{"versionId": []string{version}}.Encode()}
		req, _ := http.NewRequest(method, u.String(), nil)
		cos.AddAuthorizationHeader("test-id", "test-key", "", req, authTime)
		values, _ := url.ParseQuery(req.Header.Get("Authorization"))
		return values.Get("q-signature")
	}
	if signature("GET", parsed.Path, object.VersionID) != q.Get("q-signature") {
		t.Fatal("GET signature mismatch")
	}
	if signature("PUT", parsed.Path, object.VersionID) == q.Get("q-signature") || signature("GET", parsed.Path, "other-version") == q.Get("q-signature") || signature("GET", parsed.Path+"other", object.VersionID) == q.Get("q-signature") {
		t.Fatal("grant can change method/object/version")
	}
	for _, duration := range []time.Duration{0, -time.Second, 5*time.Minute + time.Second} {
		if _, err := s.AuthorizeOriginalRead(context.Background(), object, now.Add(duration)); err == nil {
			t.Fatal("invalid expiry accepted")
		}
	}
	for _, version := range []string{"", "null", "bad\nversion", strings.Repeat("x", 1025)} {
		invalid := object
		invalid.VersionID = version
		if _, err := s.AuthorizeOriginalRead(context.Background(), invalid, now.Add(time.Minute)); err == nil {
			t.Fatal("invalid provider version accepted")
		}
	}
	invalid := object
	invalid.Key = strings.Replace(object.Key, "/originals/", "/staging/", 1)
	if _, err := s.AuthorizeOriginalRead(context.Background(), invalid, now.Add(time.Minute)); err == nil {
		t.Fatal("staging read granted")
	}
}
