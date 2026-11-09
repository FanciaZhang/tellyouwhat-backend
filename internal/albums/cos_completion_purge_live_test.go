//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

const completionProbeScope = "albums-multipart-completion-v1"

var completionProbeModes = []string{"completion-first", "abort-body-held", "completion-request-sent", "cancel-body-held"}

type completionProbeIntent struct {
	Scope, Owner, Key, SHA256 string
	Bytes                     int64
}

func completionProbeData() []byte {
	return bytes.Repeat([]byte("disposable-albums-completion-probe-"), 512)
}

func completionProbeLease(t *testing.T, root string) *os.File {
	t.Helper()
	if os.Getenv("ALBUM_CI_LIVE_PROBE") != "1" {
		t.Skip("explicit live probe required")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private probe directory required")
	}
	lock, err := os.OpenFile(filepath.Join(root, ".completion.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		t.Fatal("probe lease unavailable")
	}
	info, err = lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		t.Fatal("probe directory is unsafe or active in another process")
	}
	t.Cleanup(func() { lock.Close() })
	return lock
}

func completionProbeCleaner(t *testing.T) *COSObjects {
	t.Helper()
	if os.Getenv("ALBUM_COS_COMPLETION_CLEANUP_PROBE") != "1" {
		t.Skip("explicit synthetic upload and version cleanup opt-in required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("home unavailable")
	}
	var keys [2]struct{ SecretId, SecretKey string }
	for i, name := range []string{"cleanup-credentials.json", "upload-cos.json"} {
		path := filepath.Join(home, ".config/tellyouwhat/albums-test", name)
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			t.Fatal("private probe credentials required")
		}
		data, e := os.ReadFile(path)
		if e != nil || json.Unmarshal(data, &keys[i]) != nil {
			t.Fatal("probe credentials unavailable")
		}
	}
	s, err := NewCOSObjects(COSConfig{Bucket: "tellyouwhat-albums-1251995834", Region: "ap-guangzhou", SecretID: keys[0].SecretId, SecretKey: keys[0].SecretKey, UploadSecretID: keys[1].SecretId, UploadSecretKey: keys[1].SecretKey})
	if err != nil {
		t.Fatal("probe cleanup configuration invalid")
	}
	return s
}

// Recover only the exact randomly named key persisted before initiation. Never
// widen deletion to an account, a prefix, or pre-existing fixture media.
func cleanCompletionProbe(t *testing.T, cleaner, reader *COSObjects, root string, intent completionProbeIntent) bool {
	t.Helper()
	parts := strings.Split(intent.Key, "/")
	data := completionProbeData()
	if intent.Scope != completionProbeScope || len(parts) != 5 || parts[0] != "albums" || parts[1] != "staging" || parts[2] != intent.Owner || !validOwner(intent.Owner) || !validOwner(parts[3]) || parts[4] != "photo" || intent.SHA256 != roundtripHash(data) || intent.Bytes != int64(len(data)) {
		t.Error("cleanup intent is not this probe's disposable resource")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	target := purgeTarget{intent.Key}
	closed, deleted := 0, 0
	passed := false
	defer func() {
		roundtripJSON(t, root, "cleanup.json", map[string]any{"passed": passed, "key": intent.Key, "sessionsClosed": closed, "versionsRemoved": deleted, "onlySyntheticKey": true})
	}()
	for range 3 {
		uploads, err := cleaner.listPurgeUploads(ctx, intent.Owner, target)
		if err != nil {
			t.Error("probe upload inventory failed; retain intent")
			return false
		}
		for _, id := range uploads {
			if cleaner.abortPurgeUpload(ctx, intent.Owner, target, id) != nil {
				t.Error("probe abort failed; retain intent")
				return false
			}
			closed++
		}
		versions, err := cleaner.listPurgeVersions(ctx, intent.Owner, target)
		if err != nil || len(versions) > 4 {
			t.Error("probe version inventory failed; retain intent")
			return false
		}
		for _, v := range versions {
			if v.DeleteMarker || v.SizeBytes != intent.Bytes {
				t.Error("unexpected probe version; preserved")
				return false
			}
			response, err := reader.client.Object.Get(ctx, intent.Key, nil, url.QueryEscape(v.VersionID))
			var content []byte
			var readErr error
			if response != nil && response.Body != nil {
				content, readErr = io.ReadAll(io.LimitReader(response.Body, intent.Bytes+1))
				response.Body.Close()
			}
			if err != nil || response == nil || response.StatusCode != 200 || response.Header.Get("x-cos-version-id") != v.VersionID || readErr != nil || !bytes.Equal(content, data) {
				t.Error("probe bytes not confirmed; preserved")
				return false
			}
		}
		for _, v := range versions {
			if cleaner.deletePurgeVersion(ctx, intent.Owner, target, v) != nil {
				t.Error("exact probe version deletion failed; retain intent")
				return false
			}
			deleted++
		}
		if len(uploads) == 0 && len(versions) == 0 {
			passed = true
			return true
		}
	}
	t.Error("probe inventory did not converge; retain intent")
	return false
}

// The real signed request is forwarded unchanged. Holding half the XML body
// lets abort run while a completion request is still being transmitted.
type completionProbeTransport struct {
	next          http.RoundTripper
	key, id, mode string
	started       chan struct{}
	release       chan struct{}
	once          sync.Once
	status        int
}

func (p *completionProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != "POST" || request.URL.Path != "/"+p.key || request.URL.Query().Get("uploadId") != p.id {
		return p.next.RoundTrip(request)
	}
	if p.mode == "abort-body-held" || p.mode == "cancel-body-held" {
		body, err := io.ReadAll(io.LimitReader(request.Body, 65537))
		request.Body.Close()
		if err != nil || len(body) < 2 || len(body) > 65536 {
			return nil, errors.New("bounded completion XML required")
		}
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		request = request.Clone(request.Context())
		request.Body = reader
		request.GetBody = nil
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			defer writer.Close()
			if _, err := writer.Write(body[:len(body)/2]); err != nil {
				return
			}
			p.once.Do(func() { close(p.started) })
			select {
			case <-p.release:
			case <-request.Context().Done():
				return
			}
			_, _ = writer.Write(body[len(body)/2:])
		}()
		response, err := p.next.RoundTrip(request)
		reader.Close()
		writer.Close()
		<-writerDone
		if response != nil {
			p.status = response.StatusCode
		}
		return response, err
	}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			p.once.Do(func() { close(p.started) })
		}
	}}))
	response, err := p.next.RoundTrip(request)
	if response != nil {
		p.status = response.StatusCode
	}
	return response, err
}

func TestLiveCOSMultipartCompletionPurgeConvergence(t *testing.T) {
	cleaner := completionProbeCleaner(t)
	s, root, ctx := liveRoundtripFixture(t)
	completionProbeLease(t, root)
	for _, mode := range completionProbeModes {
		t.Run(mode, func(t *testing.T) {
			folder := filepath.Join(root, mode)
			if os.Mkdir(folder, 0700) != nil {
				t.Fatal("fresh probe directory required; recover an existing intent first")
			}
			data := completionProbeData()
			intent := completionProbeIntent{Scope: completionProbeScope, Owner: uuid.NewString(), SHA256: roundtripHash(data), Bytes: int64(len(data))}
			intent.Key = "albums/staging/" + intent.Owner + "/" + uuid.NewString() + "/photo"
			versionsBefore, err := cleaner.listPurgeVersions(ctx, intent.Owner, purgeTarget{intent.Key})
			uploadsBefore, uploadErr := cleaner.listPurgeUploads(ctx, intent.Owner, purgeTarget{intent.Key})
			if err != nil || uploadErr != nil || len(versionsBefore) != 0 || len(uploadsBefore) != 0 {
				t.Fatal("new probe namespace is not confirmed empty; leave it untouched")
			}
			f, err := os.OpenFile(filepath.Join(folder, "intent.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal("probe intent unavailable")
			}
			err = json.NewEncoder(f).Encode(intent)
			syncErr, closeErr := f.Sync(), f.Close()
			if err != nil || syncErr != nil || closeErr != nil {
				t.Fatal("probe intent was not persisted")
			}
			t.Cleanup(func() { cleanCompletionProbe(t, cleaner, s, folder, intent) })
			id, err := s.InitiateStagingMultipart(ctx, intent.Key)
			if err != nil {
				t.Fatal("initiation failed or unknown; retain intent")
			}
			roundtripJSON(t, folder, "session.json", map[string]string{"uploadID": id})
			grant, err := s.AuthorizePart(ctx, intent.Key, id, Resource{ID: "photo", SizeBytes: intent.Bytes, SHA256: intent.SHA256}, 1, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal("part authorization failed")
			}
			request, err := http.NewRequestWithContext(ctx, "PUT", grant.URL, bytes.NewReader(data))
			if err != nil {
				t.Fatal("part request invalid")
			}
			for k, v := range grant.Headers {
				request.Header.Set(k, v)
			}
			client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			response, err := client.Do(request)
			if response != nil {
				response.Body.Close()
			}
			if err != nil || response == nil || response.StatusCode != 200 {
				t.Fatal("synthetic part was not uploaded")
			}
			gate := &completionProbeTransport{next: http.DefaultTransport, key: intent.Key, id: id, mode: mode, started: make(chan struct{}), release: make(chan struct{})}
			auth := s.archiveHTTP.Transport.(*cos.AuthorizationTransport)
			previous := auth.Transport
			auth.Transport = gate
			defer func() { auth.Transport = previous }()
			type result struct {
				version string
				err     error
			}
			done := make(chan result, 1)
			call, cancel := context.WithTimeout(ctx, 40*time.Second)
			defer cancel()
			go func() {
				v, err := s.CompleteStagingMultipart(call, intent.Key, id, intent.Bytes)
				done <- result{v, err}
			}()
			var completed result
			if mode == "completion-first" {
				completed = <-done
			} else {
				select {
				case <-gate.started:
				case completed = <-done:
					close(gate.release)
					t.Fatalf("completion ended before the intended overlap: %v", completed.err)
				case <-call.Done():
					close(gate.release)
					<-done
					t.Fatal("completion never reached the probe gate")
				}
			}
			if mode == "abort-body-held" && os.Getenv("ALBUM_COS_COMPLETION_INTERRUPT") == "1" {
				roundtripJSON(t, root, "interruption-ready.json", map[string]string{"mode": mode, "key": intent.Key})
				<-call.Done()
				close(gate.release)
				<-done
				t.Fatal("interruption probe was not stopped by its owning harness")
			}
			if mode == "cancel-body-held" {
				cancel()
				close(gate.release)
				completed = <-done
			}
			abortErr := cleaner.abortPurgeUpload(ctx, intent.Owner, purgeTarget{intent.Key}, id)
			if mode != "cancel-body-held" {
				close(gate.release)
				if mode != "completion-first" {
					completed = <-done
				}
			}
			_, missingResponse, missingErr := s.client.Object.ListParts(ctx, intent.Key, id, nil)
			if missingResponse != nil && missingResponse.Body != nil {
				missingResponse.Body.Close()
			}
			var provider *cos.ErrorResponse
			missing := missingResponse != nil && missingResponse.StatusCode == 404 && errors.As(missingErr, &provider) && provider.Code == "NoSuchUpload"
			versions, inventoryErr := cleaner.listPurgeVersions(ctx, intent.Owner, purgeTarget{intent.Key})
			unknown := errors.Is(completed.err, ErrCOSMultipartUnknown)
			roundtripJSON(t, folder, "outcome.json", map[string]any{"mode": mode, "completionHTTPStatus": gate.status, "completionSucceeded": completed.err == nil, "completionRequiresReconciliation": unknown, "concreteCompletionVersion": concreteCOSVersion(completed.version), "abortAcknowledged": abortErr == nil, "sessionMissingWithNoSuchUpload": missing, "versionsAfterBothTerminal": len(versions), "inventorySucceeded": inventoryErr == nil, "paidMediaJobs": 0, "existingMediaAccessed": false})
			if abortErr != nil || !missing || inventoryErr != nil || (completed.err != nil && !unknown) || (mode == "completion-first" && (completed.err != nil || !concreteCOSVersion(completed.version) || len(versions) != 1)) || ((mode == "abort-body-held" || mode == "cancel-body-held") && (completed.err == nil || len(versions) != 0)) || len(versions) > 1 {
				t.Error("completion/abort did not converge; inspect retained evidence")
			}
		})
	}
}

func TestLiveCOSMultipartCompletionProbeRecovery(t *testing.T) {
	if os.Getenv("ALBUM_COS_COMPLETION_RECOVER") != "1" {
		t.Skip("explicit retained-intent recovery required")
	}
	cleaner := completionProbeCleaner(t)
	reader, root, _ := liveRoundtripFixture(t)
	completionProbeLease(t, root)
	for _, mode := range completionProbeModes {
		folder := filepath.Join(root, mode)
		data, err := os.ReadFile(filepath.Join(folder, "intent.json"))
		if os.IsNotExist(err) {
			continue
		}
		var intent completionProbeIntent
		if err != nil || json.Unmarshal(data, &intent) != nil {
			t.Fatal("invalid retained probe intent")
		}
		cleanCompletionProbe(t, cleaner, reader, folder, intent)
	}
}
