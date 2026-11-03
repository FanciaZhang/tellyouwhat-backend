package albums

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

type originalSealFaultObjects struct {
	*objectTestStore
	copies, reconciles, reads int
	fault                     string
}

func (s *originalSealFaultObjects) Seal(ctx context.Context, from, to string, source OriginalSealSource) (string, error) {
	s.copies++
	if s.fault == "absent" {
		return "", ErrCOSOperation
	}
	version, err := s.objectTestStore.Seal(ctx, from, to, source)
	if s.fault == "lost-reply" {
		s.fault = ""
		return "", ErrCOSOperation
	}
	return version, err
}
func (s *originalSealFaultObjects) ReconcileSeal(ctx context.Context, key string) (string, error) {
	s.reconciles++
	return s.objectTestStore.ReconcileSeal(ctx, key)
}
func (s *originalSealFaultObjects) OpenVersion(ctx context.Context, key, version string) (io.ReadCloser, error) {
	s.reads++
	if s.fault == "read" {
		s.fault = ""
		return nil, ErrCOSOperation
	}
	return s.objectTestStore.OpenVersion(ctx, key, version)
}

func TestOriginalSealRetryNeverRepeatsCopy(t *testing.T) {
	for _, fault := range []string{"lost-reply", "read", "absent"} {
		t.Run(fault, func(t *testing.T) {
			s, r, o, owner, m := uploadFixture(t)
			clock := s.now()
			s.now = func() time.Time { return clock }
			storage := &originalSealFaultObjects{objectTestStore: o, fault: fault}
			s.objects = storage
			ctx := context.Background()
			u, err := s.Create(ctx, owner, uuid.NewString(), m)
			if err != nil {
				t.Fatal(err)
			}
			o.staging[stagingKey(u, "photo")] = []byte("original-resource-bytes")
			if _, err := s.Submit(ctx, owner, u.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Verify(ctx, owner, u.ID); err == nil {
				t.Fatal("failure hidden")
			}
			delete(o.staging, stagingKey(u, "photo"))
			clock = clock.Add(2 * time.Minute)
			recovered, err := s.Verify(ctx, owner, u.ID)
			if fault == "absent" {
				if err == nil || r.completed != 0 {
					t.Fatal("absence became backup")
				}
			} else if err != nil || recovered.State != OriginalsVerified || r.completed != 1 {
				t.Fatal("archive did not recover", err)
			}
			if storage.copies != 1 {
				t.Fatal("COPY repeated", storage.copies)
			}
			if fault == "read" && storage.reconciles != 0 {
				t.Fatal("forgot pinned archive receipt")
			}
			if fault != "read" && storage.reconciles != 1 {
				t.Fatal("did not reconcile unknown COPY", storage.reconciles)
			}
		})
	}
}

func TestOriginalSealPreflightFailureDoesNotConsumeIntent(t *testing.T) {
	s, r, o, owner, m := uploadFixture(t)
	ctx := context.Background()
	clock := s.now()
	s.now = func() time.Time { return clock }
	u, err := s.Create(ctx, owner, uuid.NewString(), m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(ctx, owner, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, owner, u.ID); err == nil {
		t.Fatal("missing staging accepted")
	}
	if _, consumed, _ := r.OriginalSeal(ctx, u, "photo"); consumed {
		t.Fatal("read failure consumed COPY")
	}
	o.staging[stagingKey(u, "photo")] = []byte("original-resource-bytes")
	clock = clock.Add(2 * time.Minute)
	if _, err := s.Verify(ctx, owner, u.ID); err != nil {
		t.Fatal("cannot recover preflight", err)
	}
}

func TestCOSOriginalSealRecoveryOnlyObservesArchive(t *testing.T) {
	owner, upload := uuid.NewString(), uuid.NewString()
	key := "albums/originals/" + owner + "/" + upload + "/photo"
	for _, kind := range []string{"found", "missing", "unversioned", "denied"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodHead || r.URL.Path != "/"+key {
					t.Fatal("recovery sent mutation or wrong target")
				}
				response := cosResponse(r, "", "archive-version")
				switch kind {
				case "missing":
					response.StatusCode = 404
				case "unversioned":
					response.Header.Del("x-cos-version-id")
				case "denied":
					response.StatusCode = 403
				}
				return response, nil
			})
			version, err := s.ReconcileSeal(context.Background(), key)
			if calls != 1 || (err == nil) != (kind == "found") || kind == "found" && version != "archive-version" {
				t.Fatal(version, err, calls)
			}
			if _, err := s.ReconcileSeal(context.Background(), "albums/staging/"+owner+"/"+upload+"/photo"); !errors.Is(err, ErrInvalidManifest) || calls != 1 {
				t.Fatal("staging used as archive", err)
			}
		})
	}
}
