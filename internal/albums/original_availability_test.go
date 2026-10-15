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

type availabilityObjects struct {
	AlbumObjects
	seen  []SealedObject
	check func(SealedObject) error
}

func (o *availabilityObjects) CheckOriginalAvailability(_ context.Context, object SealedObject) error {
	o.seen = append(o.seen, object)
	if o.check != nil {
		return o.check(object)
	}
	return nil
}

func availabilityFixture(t *testing.T) (*UploadService, *readRepository, *retainedReadRepository, *availabilityObjects) {
	t.Helper()
	s, r, _ := readFixture(t)
	// Companion resources must be checked too, not only the displayed photo.
	companion := r.version.Manifest.Resources[0]
	companion.ID, companion.Role = "companion", "alternate"
	r.version.Manifest.Resources = append(r.version.Manifest.Resources, companion)
	r.version.ManifestDigest, _ = r.version.Manifest.Digest()
	r.version.OriginalBytes += companion.SizeBytes
	object := r.version.Objects[0]
	object.ResourceID, object.Key = companion.ID, sealedKey(Upload{ID: r.version.VersionID, OwnerID: r.owner}, companion.ID)
	r.version.Objects = append(r.version.Objects, object)
	locked := &retainedReadRepository{UploadRepository: r, LibraryRepository: r}
	locked.authorize = func(ctx context.Context, owner string, v AssetVersion, now, expiry time.Time, check func(time.Time) error) error {
		if owner != r.owner || v.ManifestDigest != r.version.ManifestDigest {
			return ErrNotFound
		}
		return check(expiry)
	}
	o := &availabilityObjects{AlbumObjects: s.objects}
	s.repository, s.objects = locked, o
	return s, r, locked, o
}

func TestOriginalAvailabilityChecksCompleteOwnedVersion(t *testing.T) {
	s, r, _, o := availabilityFixture(t)
	result, err := s.CheckOriginalAvailability(context.Background(), r.owner, r.version.VersionID)
	if err != nil || len(o.seen) != 2 || result.ResourceCount != 2 || result.AssetID != r.version.AssetID ||
		result.VersionID != r.version.VersionID || result.ManifestDigest != r.version.ManifestDigest ||
		result.SourceRevision != r.version.Manifest.SourceRevision || result.OriginalBytes != r.version.OriginalBytes ||
		!result.StartedAt.Equal(s.now()) || !result.CheckedAt.Equal(s.now()) {
		t.Fatal("incomplete or unbound result", result, err)
	}
	for i := range o.seen {
		if o.seen[i] != r.version.Objects[i] {
			t.Fatal("wrong immutable version checked")
		}
	}
	for _, owner := range []string{uuid.NewString(), "invalid"} {
		if _, err := s.CheckOriginalAvailability(context.Background(), owner, r.version.VersionID); err == nil {
			t.Fatal("foreign owner accepted")
		}
	}
	if len(o.seen) != 2 {
		t.Fatal("unauthorized object I/O")
	}
}

func TestOriginalAvailabilityFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing-companion", "wrong-digest", "no-lock", "no-checker", "retention-before", "retention-after", "cancel-before", "cancel-after-first", "clock-backwards", "too-slow", "provider-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, r, locked, o := availabilityFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantCalls := 0
			switch mode {
			case "missing-companion":
				r.version.Objects = r.version.Objects[:1]
			case "wrong-digest":
				r.version.ManifestDigest = strings.Repeat("b", 64)
			case "no-lock":
				s.repository = r
			case "no-checker":
				s.objects = o.AlbumObjects
			case "retention-before":
				locked.authorize = func(context.Context, string, AssetVersion, time.Time, time.Time, func(time.Time) error) error {
					return ErrNotFound
				}
			case "retention-after":
				wantCalls = 2
				o.check = func(SealedObject) error {
					locked.authorize = func(context.Context, string, AssetVersion, time.Time, time.Time, func(time.Time) error) error {
						return ErrNotFound
					}
					return nil
				}
			case "cancel-before":
				cancel()
			case "cancel-after-first":
				wantCalls = 1
				o.check = func(SealedObject) error { cancel(); return nil }
			case "clock-backwards", "too-slow":
				wantCalls = 2
				clock := s.now()
				s.now = func() time.Time { return clock }
				o.check = func(SealedObject) error {
					if mode == "too-slow" {
						clock = clock.Add(30 * time.Second)
					} else {
						clock = clock.Add(-time.Second)
					}
					return nil
				}
			case "provider-failure":
				wantCalls = 1
				o.check = func(SealedObject) error { return ErrCOSOperation }
			}
			result, err := s.CheckOriginalAvailability(ctx, r.owner, r.version.VersionID)
			if err == nil || result != (OriginalAvailability{}) || len(o.seen) != wantCalls {
				t.Fatal("unsafe success or I/O", result, err, len(o.seen))
			}
		})
	}
}

func TestOriginalAvailabilityHTTPContract(t *testing.T) {
	s, r, _, o := availabilityFixture(t)
	router, err := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", r.owner}}, s)
	if err != nil {
		t.Fatal(err)
	}
	path := "https://albums.example/v1/albums/library/versions/" + r.version.VersionID + "/original-availability"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", path, nil))
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(response.Code, response.Body.String())
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if err := doc.Components.Schemas["OriginalAvailability"].Value.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"albums/originals", "immutable-v1", "url", "expiresAt"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatal("provider location or misleading validity exposed")
		}
	}
	for _, request := range []*http.Request{
		httptest.NewRequest("POST", path+"?owner=other", nil), httptest.NewRequest("POST", path, strings.NewReader("{}")),
		httptest.NewRequest("GET", path, nil), httptest.NewRequest("POST", strings.Replace(path, r.version.VersionID, uuid.NewString(), 1), nil),
	} {
		result := httptest.NewRecorder()
		router.ServeHTTP(result, request)
		if result.Code < 400 {
			t.Fatal("invalid request accepted")
		}
	}
	if len(o.seen) != 2 {
		t.Fatal("invalid request reached storage")
	}
}

func TestCOSOriginalAvailabilityPinsOnlineImmutableObject(t *testing.T) {
	object := SealedObject{ResourceID: "photo", Key: "albums/originals/" + uuid.NewString() + "/" + uuid.NewString() + "/photo", VersionID: "v+/=&?", SizeBytes: 23}
	for _, class := range []string{"", "STANDARD", "STANDARD_IA", "MAZ_STANDARD", "MAZ_STANDARD_IA", "ARCHIVE", "DEEP_ARCHIVE", "INTELLIGENT_TIERING", "unknown"} {
		t.Run(class, func(t *testing.T) {
			calls := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "HEAD" || r.URL.Path != "/"+object.Key || r.URL.Query().Get("versionId") != object.VersionID || r.Header.Get("Authorization") == "" {
					t.Fatal("unpinned request")
				}
				response := cosResponse(r, "", object.VersionID)
				response.ContentLength = object.SizeBytes
				response.Header.Set("x-cos-storage-class", class)
				response.Header.Set("x-cos-restore", `ongoing-request="false", expiry-date="tomorrow"`)
				return response, nil
			})
			err := s.CheckOriginalAvailability(context.Background(), object)
			allowed := class == "" || class == "STANDARD" || class == "STANDARD_IA" || class == "MAZ_STANDARD" || class == "MAZ_STANDARD_IA"
			if (err == nil) != allowed || calls != 1 {
				t.Fatal("offline or unexpected class qualified", err, calls)
			}
		})
	}
	for _, mode := range []string{"wrong-version", "wrong-size", "delete-marker", "not-found", "denied", "redirect", "transport"} {
		t.Run(mode, func(t *testing.T) {
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				response := cosResponse(r, "", object.VersionID)
				response.ContentLength = object.SizeBytes
				switch mode {
				case "wrong-version":
					response.Header.Set("x-cos-version-id", "other")
				case "wrong-size":
					response.ContentLength++
				case "delete-marker":
					response.Header.Set("x-cos-delete-marker", "true")
				case "not-found":
					response.StatusCode = 404
				case "denied":
					response.StatusCode = 403
				case "redirect":
					response.StatusCode = 302
					response.Header.Set("Location", "https://untrusted.example")
				case "transport":
					return nil, errors.New("connection failed")
				}
				return response, nil
			})
			if err := s.CheckOriginalAvailability(context.Background(), object); err == nil {
				t.Fatal("invalid storage response accepted")
			}
		})
	}
}
