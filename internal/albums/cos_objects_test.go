package albums

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type cosTestTransport func(*http.Request) (*http.Response, error)

func (f cosTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func cosResponse(r *http.Request, body, version string) *http.Response {
	h := http.Header{}
	if version != "" {
		h.Set("x-cos-version-id", version)
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r, ContentLength: int64(len(body))}
}
func cosFixture(t *testing.T, transport cosTestTransport) *COSObjects {
	t.Helper()
	s, err := newCOSObjects(COSConfig{Bucket: "albums-test-1250000000", Region: "ap-guangzhou", SecretID: "test-id", SecretKey: "test-key", UploadSecretID: "upload-test-id", UploadSecretKey: "upload-test-key"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestCOSAdapterFixesCopyAndReadVersions(t *testing.T) {
	owner, upload := uuid.NewString(), uuid.NewString()
	from := "albums/staging/" + owner + "/" + upload + "/photo"
	to := strings.Replace(from, "/staging/", "/originals/", 1)
	copies, reads := 0, 0
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.Header.Get("Authorization") == "" {
			t.Error("unsigned or insecure request")
		}
		switch r.Method {
		case http.MethodHead:
			if r.URL.Query().Get("versionId") != "" && r.URL.Query().Get("versionId") != "source-v1" {
				t.Error("wrong source version")
			}
			response := cosResponse(r, "", "source-v1")
			response.ContentLength = 23
			return response, nil
		case http.MethodPut:
			copies++
			if r.URL.Path != "/"+to || !strings.HasSuffix(r.Header.Get("x-cos-copy-source"), "?versionId=source-v1") {
				t.Errorf("unfixed copy: %s", r.Header.Get("x-cos-copy-source"))
			}
			return cosResponse(r, `<CopyObjectResult><ETag>etag</ETag></CopyObjectResult>`, "archive-v2"), nil
		case http.MethodGet:
			reads++
			if r.URL.Path != "/"+to || r.URL.Query().Get("versionId") != "archive-v2" {
				t.Error("read latest instead of sealed version")
			}
			return cosResponse(r, "original-resource-bytes", "archive-v2"), nil
		default:
			t.Fatalf("unexpected method %s", r.Method)
			return nil, nil
		}
	})
	version, err := s.Seal(context.Background(), from, to)
	if err != nil || version != "archive-v2" {
		t.Fatalf("seal %q %v", version, err)
	}
	body, err := s.OpenVersion(context.Background(), to, version)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(data) != "original-resource-bytes" || copies != 1 || reads != 1 {
		t.Fatalf("read %q %v copies=%d reads=%d", data, err, copies, reads)
	}
}

func TestCOSGrantsCannotWriteArchiveAndBindSize(t *testing.T) {
	s := cosFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("signing should not contact storage")
		return nil, nil
	})
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/photo"
	resource := Resource{ID: "photo", SizeBytes: 123}
	grant, err := s.AuthorizePut(context.Background(), key, resource, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := url.Parse(grant.URL)
	if err != nil {
		t.Fatal(err)
	}
	auth, _ := url.QueryUnescape(signed.RawQuery)
	if signed.Query().Get("q-ak") != "upload-test-id" {
		t.Fatal("archive credentials used for client grant")
	}
	if !strings.Contains(auth, "content-length") || !strings.Contains(auth, "content-type") || grant.Headers["Content-Length"] != "123" {
		t.Fatalf("upload size/type not signed")
	}
	for _, invalid := range []string{strings.Replace(key, "/staging/", "/originals/", 1), key + "/../photo", strings.Replace(key, "/photo", "/different", 1)} {
		if _, err := s.AuthorizePut(context.Background(), invalid, resource, time.Now().Add(time.Minute)); err == nil {
			t.Fatal("invalid write path accepted")
		}
	}
}

func TestCOSUnversionedSourceAndMismatchedReadFailClosed(t *testing.T) {
	from := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/photo"
	to := strings.Replace(from, "/staging/", "/originals/", 1)
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) { return cosResponse(r, "bytes", "null"), nil })
	if _, err := s.Seal(context.Background(), from, to); !errors.Is(err, ErrIncompleteBackup) {
		t.Fatalf("unversioned source: %v", err)
	}
	if _, err := s.OpenVersion(context.Background(), to, "requested-version"); !errors.Is(err, ErrIncompleteBackup) {
		t.Fatalf("wrong response version: %v", err)
	}
	if _, err := s.OpenVersion(context.Background(), to, "null"); !errors.Is(err, ErrIncompleteBackup) {
		t.Fatalf("null version: %v", err)
	}
}

func TestCOSLargeCopyPinsEveryPartToSourceVersion(t *testing.T) {
	from := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/video"
	to := strings.Replace(from, "/staging/", "/originals/", 1)
	var parts atomic.Int64
	var completed atomic.Bool
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case http.MethodHead:
			response := cosResponse(r, "", "large-source-v1")
			response.ContentLength = 6 << 30
			return response, nil
		case http.MethodPost:
			if r.URL.Query().Has("uploads") {
				return cosResponse(r, `<InitiateMultipartUploadResult><UploadId>copy-upload</UploadId></InitiateMultipartUploadResult>`, ""), nil
			}
			if r.URL.Query().Get("uploadId") != "copy-upload" {
				t.Error("unexpected complete upload ID")
			}
			completed.Store(true)
			return cosResponse(r, `<CompleteMultipartUploadResult><ETag>final-etag</ETag></CompleteMultipartUploadResult>`, "large-archive-v2"), nil
		case http.MethodPut:
			if r.URL.Query().Get("uploadId") != "copy-upload" || !strings.HasSuffix(r.Header.Get("x-cos-copy-source"), "?versionId=large-source-v1") || r.Header.Get("x-cos-copy-source-range") == "" {
				t.Error("multipart source was not pinned or range missing")
			}
			parts.Add(1)
			return cosResponse(r, `<CopyPartResult><ETag>part-etag</ETag></CopyPartResult>`, ""), nil
		default:
			t.Errorf("unexpected method %s", r.Method)
			return nil, errors.New("unexpected test request")
		}
	})
	version, err := s.Seal(context.Background(), from, to)
	if err != nil || version != "large-archive-v2" || parts.Load() != 96 || !completed.Load() {
		t.Fatalf("large copy version=%q parts=%d completed=%v err=%v", version, parts.Load(), completed.Load(), err)
	}
}

func TestCOSCopyEmbeddedErrorAndDisabledVersioningAreRejected(t *testing.T) {
	from := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/photo"
	to := strings.Replace(from, "/staging/", "/originals/", 1)
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Has("versioning") {
			return cosResponse(r, `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`, ""), nil
		}
		if r.Method == http.MethodHead {
			response := cosResponse(r, "", "source-v1")
			response.ContentLength = 23
			return response, nil
		}
		return cosResponse(r, `<Error><Code>InternalError</Code><Message>copy failed</Message></Error>`, ""), nil
	})
	if err := s.Check(context.Background()); !errors.Is(err, ErrIncompleteBackup) {
		t.Fatalf("suspended versioning: %v", err)
	}
	if _, err := s.Seal(context.Background(), from, to); !errors.Is(err, ErrCOSOperation) {
		t.Fatalf("HTTP 200 copy error accepted: %v", err)
	}
}

func TestCOSMultipartFailureAbortsWithIndependentContext(t *testing.T) {
	for _, mode := range []string{"part", "cancel", "complete", "abort"} {
		t.Run(mode, func(t *testing.T) {
			from := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/video"
			to := strings.Replace(from, "/staging/", "/originals/", 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var aborts atomic.Int64
			var completes atomic.Int64
			failure := func(r *http.Request) *http.Response {
				result := cosResponse(r, `<Error><Code>InvalidRequest</Code><Message>test rejection</Message></Error>`, "")
				result.StatusCode = http.StatusBadRequest
				return result
			}
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				switch r.Method {
				case http.MethodHead:
					result := cosResponse(r, "", "source-version")
					result.ContentLength = 6 << 30
					return result, nil
				case http.MethodPost:
					if r.URL.Query().Has("uploads") {
						return cosResponse(r, `<InitiateMultipartUploadResult><UploadId>abort-this-upload</UploadId></InitiateMultipartUploadResult>`, ""), nil
					}
					completes.Add(1)
					return failure(r), nil
				case http.MethodPut:
					if mode == "cancel" {
						cancel()
						return nil, context.Canceled
					}
					if mode != "complete" {
						return failure(r), nil
					}
					return cosResponse(r, `<CopyPartResult><ETag>etag</ETag></CopyPartResult>`, ""), nil
				case http.MethodDelete:
					aborts.Add(1)
					if r.Context().Err() != nil {
						t.Error("abort inherited cancelled context")
					}
					if _, ok := r.Context().Deadline(); !ok {
						t.Error("unbounded cleanup")
					}
					if r.URL.Path != "/"+to || r.URL.Query().Get("uploadId") != "abort-this-upload" {
						t.Error("aborting unrelated upload")
					}
					if mode == "abort" {
						return failure(r), nil
					}
					result := cosResponse(r, "", "")
					result.StatusCode = http.StatusNoContent
					return result, nil
				default:
					return nil, errors.New("unexpected request")
				}
			})
			version, err := s.Seal(ctx, from, to)
			if err == nil || version != "" || aborts.Load() != 1 {
				t.Fatalf("failure version=%q err=%v aborts=%d", version, err, aborts.Load())
			}
			if mode != "complete" && completes.Load() != 0 {
				t.Error("completed failed parts")
			}
			if errors.Is(err, ErrCOSCleanup) != (mode == "abort") {
				t.Fatalf("cleanup outcome lost: %v", err)
			}
		})
	}
}
