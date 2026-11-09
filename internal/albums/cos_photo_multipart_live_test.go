//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

const photoMultipartProbeScope = "albums-photo-multipart-v1"

type photoMultipartProbeIntent struct {
	Scope string
	Spec  PhotoWriteSpec
}

func photoMultipartProbeCandidate(t *testing.T) PhotoCandidate {
	t.Helper()
	candidate, err := inspectPhotoCandidate(candidateImage(t, "jpeg", 47), PhotoGridJPEG)
	if err != nil {
		t.Fatal("synthetic photo invalid")
	}
	return candidate
}

func photoMultipartProbeCheckpoint(t *testing.T, root, name string, value any) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("fresh durable photo probe checkpoint required")
	}
	err = json.NewEncoder(f).Encode(value)
	synced, closed := f.Sync(), f.Close()
	if err != nil || synced != nil || closed != nil {
		t.Fatal("photo probe checkpoint not durable; retain intent")
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal("photo probe directory unavailable")
	}
	synced, closed = dir.Sync(), dir.Close()
	if synced != nil || closed != nil {
		t.Fatal("photo probe directory not durable; retain intent")
	}
}

// Capture operation/status/error code only, never signed headers, URLs or media.
func photoMultipartProbeTrace(t *testing.T, s *COSObjects) func() []map[string]any {
	t.Helper()
	auth, ok := s.archiveHTTP.Transport.(*cos.AuthorizationTransport)
	if !ok {
		t.Fatal("unexpected photo probe transport")
	}
	next := auth.Transport
	t.Cleanup(func() { auth.Transport = next })
	trace := []map[string]any{}
	var mutex sync.Mutex
	auth.Transport = cosTestTransport(func(request *http.Request) (*http.Response, error) {
		response, err := next.RoundTrip(request)
		entry := map[string]any{"method": request.Method, "multipart": request.URL.Query().Has("uploadId"), "part": request.URL.Query().Has("partNumber")}
		if response != nil {
			entry["status"] = response.StatusCode
			if response.StatusCode >= 400 && response.Body != nil {
				body := response.Body
				data, readErr := io.ReadAll(io.LimitReader(body, 65537))
				response.Body = struct {
					io.Reader
					io.Closer
				}{io.MultiReader(bytes.NewReader(data), body), body}
				var failure struct{ Code string }
				if readErr == nil && len(data) <= 65536 && xml.Unmarshal(data, &failure) == nil {
					entry["code"] = failure.Code
				}
			}
		}
		mutex.Lock()
		trace = append(trace, entry)
		mutex.Unlock()
		return response, err
	})
	return func() []map[string]any {
		mutex.Lock()
		defer mutex.Unlock()
		return append([]map[string]any(nil), trace...)
	}
}

// Only this persisted, unique synthetic key is eligible for cleanup. A possibly
// issued Complete needs a positive receipt; absence alone never authorizes it.
func cleanPhotoMultipartProbe(t *testing.T, cleaner, reader *COSObjects, root string, intent photoMultipartProbeIntent) bool {
	t.Helper()
	candidate := photoMultipartProbeCandidate(t)
	return cleanPhotoMultipartCandidateProbe(t, cleaner, reader, root, intent, candidate, PhotoGridJPEG)
}

func cleanPhotoMultipartCandidateProbe(t *testing.T, cleaner, reader *COSObjects, root string, intent photoMultipartProbeIntent, candidate PhotoCandidate, preset PhotoPreset) bool {
	t.Helper()
	key, err := intent.Spec.key()
	if err != nil || intent.Scope != photoMultipartProbeScope || !intent.Spec.Multipart || intent.Spec.Preset != preset ||
		intent.Spec.SHA256 != candidate.SHA256 || intent.Spec.SizeBytes != int64(len(candidate.Bytes)) || intent.Spec.Width != candidate.Width || intent.Spec.Height != candidate.Height {
		t.Error("photo cleanup intent rejected")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	passed, deleted := false, 0
	defer func() {
		roundtripJSON(t, root, "cleanup.json", map[string]any{"passed": passed, "versionsRemoved": deleted, "onlySyntheticKey": true})
	}()
	target := purgeTarget{key}
	var receipt SealedObject
	_, markerErr := os.Lstat(filepath.Join(root, "completion-request.json"))
	if markerErr != nil && !os.IsNotExist(markerErr) {
		t.Error("completion checkpoint unavailable; retained")
		return false
	}
	if markerErr == nil {
		data, readErr := os.ReadFile(filepath.Join(root, "receipt.json"))
		if readErr == nil {
			if json.Unmarshal(data, &receipt) != nil || receipt.Key != key || receipt.SHA256 != intent.Spec.SHA256 || receipt.SizeBytes != intent.Spec.SizeBytes || !concreteCOSVersion(receipt.VersionID) {
				t.Error("photo receipt invalid; retained")
				return false
			}
		} else if os.IsNotExist(readErr) {
			receipt, err = reader.ReconcilePhotoCandidate(ctx, intent.Spec)
			if err != nil {
				t.Error("photo completion still uncertain; retained")
				return false
			}
			photoMultipartProbeCheckpoint(t, root, "receipt.json", receipt)
		} else {
			t.Error("photo receipt unavailable; retained")
			return false
		}
	} else {
		var session struct{ UploadID string }
		data, readErr := os.ReadFile(filepath.Join(root, "session.json"))
		if os.IsNotExist(readErr) {
			session.UploadID, err = reader.FindPhotoMultipart(ctx, intent.Spec)
			if err != nil {
				t.Error("photo initialization still uncertain; retained")
				return false
			}
			photoMultipartProbeCheckpoint(t, root, "session.json", session)
		} else if readErr != nil || json.Unmarshal(data, &session) != nil || !validCOSMultipartID(session.UploadID) {
			t.Error("photo session checkpoint unavailable; retained")
			return false
		}
		p := PhotoWriteRecord{Spec: intent.Spec, MultipartID: session.UploadID, MultipartPhase: "ready", RetirementStartedAt: time.Now().UTC()}
		if reader.AbortPhotoMultipart(ctx, p) != nil {
			t.Error("photo session termination unknown; retained")
			return false
		}
	}
	uploads, err := cleaner.listPurgeUploads(ctx, intent.Spec.OwnerID, target)
	versions, versionErr := cleaner.listPurgeVersions(ctx, intent.Spec.OwnerID, target)
	if err != nil || versionErr != nil || len(uploads) != 0 || len(versions) > 1 || markerErr != nil && len(versions) != 0 {
		t.Error("unexpected photo objects or sessions; retained")
		return false
	}
	for _, version := range versions {
		if version.DeleteMarker || version.SizeBytes != intent.Spec.SizeBytes || version.VersionID != receipt.VersionID {
			t.Error("unexpected photo version; retained")
			return false
		}
		if _, err := reader.verifyPhotoWrite(ctx, intent.Spec, key, version.VersionID); err != nil || cleaner.deletePurgeVersion(ctx, intent.Spec.OwnerID, target, version) != nil {
			t.Error("photo version verification or deletion failed; retained")
			return false
		}
		deleted++
	}
	uploads, err = cleaner.listPurgeUploads(ctx, intent.Spec.OwnerID, target)
	versions, versionErr = cleaner.listPurgeVersions(ctx, intent.Spec.OwnerID, target)
	if err != nil || versionErr != nil || len(uploads) != 0 || len(versions) != 0 {
		t.Error("photo cleanup not converged; retained")
		return false
	}
	passed = true
	return true
}

// Explicit cloud storage probe, not an App account/subscription journey. It
// creates at most two tiny synthetic JPEGs and never calls CI or reads user media.
func TestLiveCOSPhotoMultipartLifecycle(t *testing.T) {
	cleaner := completionProbeCleaner(t)
	s, root, ctx := liveRoundtripFixture(t)
	completionProbeLease(t, root)
	for _, mode := range []string{"complete", "abort-before-complete"} {
		if !t.Run(mode, func(t *testing.T) {
			folder := filepath.Join(root, mode)
			if os.Mkdir(folder, 0700) != nil {
				t.Fatal("fresh photo probe directory required; recover retained intent")
			}
			candidate := photoMultipartProbeCandidate(t)
			intent := photoMultipartProbeIntent{Scope: photoMultipartProbeScope, Spec: PhotoWriteSpec{OwnerID: uuid.NewString(), AttemptID: uuid.NewString(), Preset: PhotoGridJPEG, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height, Multipart: true}}
			key, _ := intent.Spec.key()
			versions, err := cleaner.listPurgeVersions(ctx, intent.Spec.OwnerID, purgeTarget{key})
			uploads, uploadErr := cleaner.listPurgeUploads(ctx, intent.Spec.OwnerID, purgeTarget{key})
			if err != nil || uploadErr != nil || len(versions) != 0 || len(uploads) != 0 {
				t.Fatal("new photo probe key not confirmed empty")
			}
			photoMultipartProbeCheckpoint(t, folder, "intent.json", intent)
			trace := photoMultipartProbeTrace(t, s)
			t.Cleanup(func() { roundtripJSON(t, folder, "http.json", trace()) })
			t.Cleanup(func() { cleanPhotoMultipartProbe(t, cleaner, s, folder, intent) })
			id, err := s.StartPhotoMultipart(ctx, intent.Spec)
			if err != nil {
				t.Fatal("photo initialization unknown; retain intent and do not reinitialize")
			}
			photoMultipartProbeCheckpoint(t, folder, "session.json", map[string]string{"uploadID": id})
			beforeComplete := func(context.Context) error {
				if mode == "abort-before-complete" {
					return ErrDerivedRetired
				}
				photoMultipartProbeCheckpoint(t, folder, "completion-request.json", map[string]bool{"mayComplete": true})
				return nil
			}
			receipt, err := s.StorePhotoMultipartOnce(ctx, intent.Spec, id, candidate, beforeComplete)
			if mode == "abort-before-complete" {
				if err != ErrDerivedRetired {
					t.Fatal("photo part did not reach the retirement fence")
				}
			} else {
				if err != nil {
					t.Fatal("photo write unresolved; inspect HTTP evidence before retry")
				}
				photoMultipartProbeCheckpoint(t, folder, "receipt.json", receipt)
			}
		}) {
			return
		}
	}
}

func TestLiveCOSPhotoMultipartProbeRecovery(t *testing.T) {
	if os.Getenv("ALBUM_COS_COMPLETION_RECOVER") != "1" {
		t.Skip("explicit photo probe recovery required")
	}
	cleaner := completionProbeCleaner(t)
	reader, root, _ := liveRoundtripFixture(t)
	completionProbeLease(t, root)
	trace := photoMultipartProbeTrace(t, reader)
	t.Cleanup(func() { roundtripJSON(t, root, "recovery-http.json", trace()) })
	found := 0
	for _, mode := range []string{"complete", "abort-before-complete", "sql-retirement-first", "sql-completion-first"} {
		folder := filepath.Join(root, mode)
		info, err := os.Lstat(folder)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			t.Fatal("private owned photo probe folder required")
		}
		info, err = os.Lstat(filepath.Join(folder, "intent.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 8192 {
			t.Fatal("bounded private photo intent required")
		}
		found++
		var intent photoMultipartProbeIntent
		roundtripRead(t, folder, "intent.json", &intent)
		cleanPhotoMultipartProbe(t, cleaner, reader, folder, intent)
	}
	if found == 0 {
		t.Fatal("no retained photo probe intent found")
	}
}

func TestPhotoMultipartProbeUsesReaderForTermination(t *testing.T) {
	root := t.TempDir()
	candidate := photoMultipartProbeCandidate(t)
	intent := photoMultipartProbeIntent{Scope: photoMultipartProbeScope, Spec: PhotoWriteSpec{OwnerID: uuid.NewString(), AttemptID: uuid.NewString(), Preset: PhotoGridJPEG, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height, Multipart: true}}
	key, _ := intent.Spec.key()
	photoMultipartProbeCheckpoint(t, root, "session.json", map[string]string{"uploadID": "bound-photo-session"})
	readerCalls, cleanerCalls := 0, 0
	reader := cosFixture(t, func(request *http.Request) (*http.Response, error) {
		readerCalls++
		if request.URL.Path != "/"+key || request.URL.Query().Get("uploadId") != "bound-photo-session" || (request.Method != "DELETE" && request.Method != "GET") {
			t.Fatal("reader escaped the bound session")
		}
		response := cosResponse(request, `<Error><Code>NoSuchUpload</Code></Error>`, "")
		response.StatusCode = http.StatusNotFound
		return response, nil
	})
	cleaner := cosFixture(t, func(request *http.Request) (*http.Response, error) {
		cleanerCalls++
		q := request.URL.Query()
		if request.Method != "GET" || request.URL.Path != "/" || q.Get("prefix") != key {
			t.Fatal("cleanup identity was used to read or write session contents")
		}
		if q.Has("uploads") {
			return cosResponse(request, `<ListMultipartUploadsResult><Bucket>albums-test-1250000000</Bucket><MaxUploads>1000</MaxUploads><Prefix>`+key+`</Prefix><IsTruncated>false</IsTruncated></ListMultipartUploadsResult>`, ""), nil
		}
		if !q.Has("versions") || !strings.HasPrefix(key, "albums/derivatives/"+intent.Spec.OwnerID+"/") {
			t.Fatal("unbounded cleanup inventory")
		}
		return videoResponse(t, request, cos.BucketGetObjectVersionsResult{Name: "albums-test-1250000000", Prefix: key}), nil
	})
	if !cleanPhotoMultipartProbe(t, cleaner, reader, root, intent) || readerCalls != 2 || cleanerCalls != 4 {
		t.Fatal("probe identity separation regressed", readerCalls, cleanerCalls)
	}
}
