package albums

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This is a service test double, not evidence for MySQL's transactional behavior.
type uploadTestRepository struct {
	mu        sync.Mutex
	values    map[string]Upload
	completed int
}

func (r *uploadTestRepository) Reserve(_ context.Context, u Upload) (Upload, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, old := range r.values {
		if old.OwnerID == u.OwnerID && old.RequestID == u.RequestID {
			if old.Digest != u.Digest {
				return Upload{}, ErrConflict
			}
			return old, nil
		}
	}
	r.values[u.ID] = u
	return u, nil
}
func (r *uploadTestRepository) Get(_ context.Context, owner, id string) (Upload, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.values[id]
	if !ok || u.OwnerID != owner {
		return Upload{}, ErrNotFound
	}
	return u, nil
}
func (r *uploadTestRepository) Submit(_ context.Context, owner, id string, now time.Time) (Upload, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.values[id]
	if !ok || u.OwnerID != owner {
		return Upload{}, ErrNotFound
	}
	if u.State == OriginalsVerified || u.State == Queued || u.State == Verifying {
		return u, nil
	}
	if !now.Before(u.ExpiresAt) {
		return Upload{}, ErrExpired
	}
	u.State = Queued
	r.values[id] = u
	return u, nil
}
func (r *uploadTestRepository) Claim(_ context.Context, owner, id, token string, now, until time.Time) (Upload, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.values[id]
	if !ok || u.OwnerID != owner {
		return Upload{}, ErrNotFound
	}
	if u.State == OriginalsVerified {
		return u, nil
	}
	if !now.Before(u.ExpiresAt) {
		return Upload{}, ErrExpired
	}
	if u.State != Queued && !(u.State == Verifying && !now.Before(u.LeaseUntil)) {
		return Upload{}, ErrLease
	}
	u.State = Verifying
	u.LeaseToken = token
	u.LeaseUntil = until
	r.values[id] = u
	return u, nil
}
func (r *uploadTestRepository) Complete(_ context.Context, owner, id, token string, objects []SealedObject, now time.Time) (Upload, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.values[id]
	if !ok || u.OwnerID != owner {
		return Upload{}, ErrNotFound
	}
	if u.State == OriginalsVerified {
		return u, nil
	}
	if u.State != Verifying || u.LeaseToken != token || !now.Before(u.LeaseUntil) {
		return Upload{}, ErrLease
	}
	u.State = OriginalsVerified
	u.Objects = objects
	r.values[id] = u
	r.completed++
	return u, nil
}

type objectTestStore struct {
	staging      map[string][]byte
	sealed       map[string][]byte
	grantKeys    []string
	afterSeal    func()
	emptyVersion bool
}

func (s *objectTestStore) AuthorizePut(_ context.Context, key string, _ Resource, expiry time.Time) (PutGrant, error) {
	s.grantKeys = append(s.grantKeys, key)
	return PutGrant{URL: "https://storage.invalid/temporary-grant", ExpiresAt: expiry}, nil
}
func (s *objectTestStore) Seal(_ context.Context, from, to string) (string, error) {
	data, ok := s.staging[from]
	if !ok {
		return "", ErrNotFound
	}
	if _, ok := s.sealed[to]; !ok {
		s.sealed[to] = bytes.Clone(data)
	}
	if s.afterSeal != nil {
		s.afterSeal()
	}
	if s.emptyVersion {
		return "", nil
	}
	return "immutable-v1", nil
}
func (s *objectTestStore) OpenVersion(_ context.Context, key, version string) (io.ReadCloser, error) {
	if version != "immutable-v1" {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(s.sealed[key])), nil
}

func uploadFixture(t *testing.T) (*UploadService, *uploadTestRepository, *objectTestStore, string, Manifest) {
	t.Helper()
	data := []byte("original-resource-bytes")
	hash := sha256.Sum256(data)
	m := Manifest{AssetID: "asset", SourceRevision: "revision-1", Kind: Photo, Resources: []Resource{{ID: "photo", Role: OriginalPhoto, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}}}
	r := &uploadTestRepository{values: map[string]Upload{}}
	o := &objectTestStore{staging: map[string][]byte{}, sealed: map[string][]byte{}}
	s, err := NewUploadService(r, o, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, func() time.Time { return time.Unix(1790000000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return s, r, o, uuid.NewString(), m
}

func TestUploadReceiptCannotVerifyAndOtherAccountCannotRead(t *testing.T) {
	s, _, objects, owner, m := uploadFixture(t)
	ctx := context.Background()
	u, err := s.Create(ctx, owner, uuid.NewString(), m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, uuid.NewString(), u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account read: %v", err)
	}
	if _, err := s.Grants(ctx, uuid.NewString(), u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account grant: %v", err)
	}
	grants, err := s.Grants(ctx, owner, u.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants: %+v %v", grants, err)
	}
	if !strings.HasPrefix(objects.grantKeys[0], "albums/staging/"+owner+"/") {
		t.Fatal("client granted non-staging key")
	}
	queued, err := s.Submit(ctx, owner, u.ID)
	if err != nil || queued.State != Queued {
		t.Fatalf("submit: %+v %v", queued, err)
	}
	if _, err := s.Grants(ctx, owner, u.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("grant after submit: %v", err)
	}
	if _, err := s.Verify(ctx, owner, u.ID); err == nil {
		t.Fatal("missing bytes became verified")
	}
	stored, _ := s.Get(ctx, owner, u.ID)
	if stored.State == OriginalsVerified {
		t.Fatal("client receipt was trusted")
	}
}

func TestVerificationReadsSealedVersionAndCompletionIsIdempotent(t *testing.T) {
	s, r, objects, owner, m := uploadFixture(t)
	ctx := context.Background()
	u, err := s.Create(ctx, owner, uuid.NewString(), m)
	if err != nil {
		t.Fatal(err)
	}
	key := stagingKey(u, "photo")
	objects.staging[key] = []byte("original-resource-bytes")
	objects.afterSeal = func() { objects.staging[key] = []byte("overwritten-by-client") }
	if _, err := s.Submit(ctx, owner, u.ID); err != nil {
		t.Fatal(err)
	}
	verified, err := s.Verify(ctx, owner, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if verified.State != OriginalsVerified || len(verified.Objects) != 1 || verified.Objects[0].VersionID == "" {
		t.Fatalf("invalid proof: %+v", verified)
	}
	if _, err := s.Verify(ctx, owner, u.ID); err != nil {
		t.Fatal(err)
	}
	if r.completed != 1 {
		t.Fatalf("completion replayed %d times", r.completed)
	}
}

func TestWrongBytesSizeOrUnversionedObjectNeverBecomeBackup(t *testing.T) {
	for _, test := range []struct {
		name        string
		data        []byte
		unversioned bool
	}{
		{"wrong hash", bytes.Repeat([]byte{'x'}, len("original-resource-bytes")), false},
		{"too large", bytes.Repeat([]byte{1}, 1000), false},
		{"too short", []byte{1}, false},
		{"no version", []byte("original-resource-bytes"), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, r, o, owner, m := uploadFixture(t)
			ctx := context.Background()
			u, err := s.Create(ctx, owner, uuid.NewString(), m)
			if err != nil {
				t.Fatal(err)
			}
			o.staging[stagingKey(u, "photo")] = test.data
			o.emptyVersion = test.unversioned
			if _, err := s.Submit(ctx, owner, u.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Verify(ctx, owner, u.ID); !errors.Is(err, ErrIncompleteBackup) {
				t.Fatalf("expected incomplete, got %v", err)
			}
			if r.completed != 0 {
				t.Fatal("unverified object committed")
			}
		})
	}
}

func TestUploadRequestReplayCannotChangeManifest(t *testing.T) {
	s, _, _, owner, m := uploadFixture(t)
	ctx := context.Background()
	request := uuid.NewString()
	a, err := s.Create(ctx, owner, request, m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, owner, request, m)
	if err != nil || a.ID != b.ID {
		t.Fatalf("non-idempotent replay: %v", err)
	}
	m.SourceRevision = "edited"
	if _, err := s.Create(ctx, owner, request, m); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay: %v", err)
	}
}

type testAccountAuth struct {
	identity AccountIdentity
	err      error
}

func (a testAccountAuth) Authenticate(context.Context, *http.Request) (AccountIdentity, error) {
	return a.identity, a.err
}

func TestUploadHTTPRejectsHostAppAndClientOwnerInjection(t *testing.T) {
	s, _, _, owner, m := uploadFixture(t)
	auth := testAccountAuth{identity: AccountIdentity{"albums", owner}}
	router, err := NewHTTPRouter("albums.example", auth, s)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"requestID": uuid.NewString(), "manifest": m, "ownerID": uuid.NewString()})
	request := httptest.NewRequest(http.MethodPost, "https://albums.example/v1/albums/uploads", bytes.NewReader(body))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("owner injection status %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "https://other.example/v1/albums/uploads/"+uuid.NewString(), nil)
	request.Header.Set("X-Forwarded-Host", "albums.example")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatal("forwarded host bypassed bound host")
	}
	bad, _ := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"health", owner}}, s)
	response = httptest.NewRecorder()
	bad.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://albums.example/v1/albums/uploads/"+uuid.NewString(), nil))
	if response.Code != http.StatusForbidden {
		t.Fatal("cross-app account accepted")
	}
	if _, err := NewHTTPRouter("albums.example", nil, s); err == nil {
		t.Fatal("missing auth accepted")
	}
}

func TestUploadHTTPRequiresExplicitEditStateAndQueuesWithoutBackupClaim(t *testing.T) {
	s, repo, _, owner, manifest := uploadFixture(t)
	router, err := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", owner}}, s)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(manifest)
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"missing", "null", "edited_without_render"} {
		t.Run(state, func(t *testing.T) {
			delete(wire, "edited")
			if state == "null" {
				wire["edited"] = nil
			}
			if state == "edited_without_render" {
				wire["edited"] = true
			}
			body, _ := json.Marshal(map[string]any{"requestID": uuid.NewString(), "manifest": wire})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "https://albums.example/v1/albums/uploads", bytes.NewReader(body)))
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			if len(repo.values) != 0 {
				t.Fatal("invalid edit state reserved quota")
			}
		})
	}
	body, _ := json.Marshal(map[string]any{"requestID": uuid.NewString(), "manifest": manifest})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "https://albums.example/v1/albums/uploads", bytes.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create %d: %s", response.Code, response.Body.String())
	}
	var upload Upload
	if err := json.Unmarshal(response.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/submit", ""} {
		method := http.MethodPost
		want := http.StatusAccepted
		if suffix == "" {
			method = http.MethodGet
			want = http.StatusOK
		}
		response = httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(method, "https://albums.example/v1/albums/uploads/"+upload.ID+suffix, nil))
		if response.Code != want {
			t.Fatalf("%s: %d", suffix, response.Code)
		}
		if err := json.Unmarshal(response.Body.Bytes(), &upload); err != nil {
			t.Fatal(err)
		}
		if upload.State != Queued {
			t.Fatalf("client submit claimed backup: %s", upload.State)
		}
	}
}
