package albums

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMultipartLayoutCoversExactResourceWithoutOverflow(t *testing.T) {
	for _, size := range []int64{1, 8 << 20, (8 << 20) + 1, 10000*(8<<20) + 1, 10000 * (5 << 30)} {
		layout, err := NewMultipartLayout(size)
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for number := 1; number <= layout.PartCount; number++ {
			offset, length, err := layout.Part(number)
			if err != nil || offset != total || length <= 0 || length > 5<<30 {
				t.Fatal("invalid geometry")
			}
			total += length
		}
		if total != size || layout.PartCount > 10000 {
			t.Fatal("wrong total")
		}
		if _, _, err := layout.Part(layout.PartCount + 1); err == nil {
			t.Fatal("extra part accepted")
		}
	}
	for _, size := range []int64{0, -1, math.MaxInt64} {
		if _, err := NewMultipartLayout(size); err == nil {
			t.Fatal("invalid size accepted")
		}
	}
}
func TestCOSPartGrantSignsExactQueryAndLength(t *testing.T) {
	s := cosFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("signing should not send network request")
		return nil, nil
	})
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/video"
	grant, err := s.AuthorizePart(context.Background(), key, "provider-upload", Resource{ID: "video", SizeBytes: (8 << 20) + 17}, 2, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(grant.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("uploadId") != "provider-upload" || query.Get("partNumber") != "2" || grant.Offset != 8<<20 || grant.SizeBytes != 17 || grant.Headers["Content-Length"] != "17" {
		t.Fatal("wrong part binding")
	}
	if !strings.Contains(query.Get("q-url-param-list"), "partnumber") || !strings.Contains(query.Get("q-url-param-list"), "uploadid") || !strings.Contains(query.Get("q-header-list"), "content-length") {
		t.Fatal("unsigned upload scope", query.Get("q-url-param-list"))
	}
	if _, err := s.AuthorizePart(context.Background(), key, "bad&id=other", Resource{ID: "video", SizeBytes: 1}, 1, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("query injection accepted")
	}
}
func TestCOSMultipartCompletionUsesProviderPartsAndVersion(t *testing.T) {
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/video"
	for _, bad := range []string{"", "missing", "wrong-size", "wrong-id", "bad-marker"} {
		t.Run(bad, func(t *testing.T) {
			completed := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/"+key || r.URL.Query().Get("uploadId") != "provider-upload" {
					t.Fatal("wrong target")
				}
				if r.Method == "GET" {
					uploadID := "provider-upload"
					if bad == "wrong-id" {
						uploadID = "other"
					}
					size := int64(8 << 20)
					if bad == "wrong-size" {
						size--
					}
					parts := fmt.Sprintf(`<Part><PartNumber>1</PartNumber><ETag>etag-one</ETag><Size>%d</Size></Part>`, size)
					if bad != "missing" {
						parts += `<Part><PartNumber>2</PartNumber><ETag>etag-two</ETag><Size>17</Size></Part>`
					}
					tail := ""
					if bad == "bad-marker" {
						tail = `<IsTruncated>true</IsTruncated><NextPartNumberMarker>0</NextPartNumberMarker>`
					}
					return cosResponse(r, fmt.Sprintf(`<ListPartsResult><Key>%s</Key><UploadId>%s</UploadId>%s%s</ListPartsResult>`, key, uploadID, parts, tail), ""), nil
				}
				if r.Method == "POST" {
					completed++
					body, _ := io.ReadAll(r.Body)
					if strings.Contains(string(body), "<Size>") {
						t.Fatal("completion must only send part numbers and ETags")
					}
					if !strings.Contains(string(body), "etag-one") || !strings.Contains(string(body), "etag-two") {
						t.Fatal("completion lost provider ETags")
					}
					return cosResponse(r, `<CompleteMultipartUploadResult><ETag>assembled</ETag></CompleteMultipartUploadResult>`, "stage-version"), nil
				}
				t.Fatal("unexpected method")
				return nil, nil
			})
			version, err := s.CompleteStagingMultipart(context.Background(), key, "provider-upload", (8<<20)+17)
			if bad == "" {
				if err != nil || version != "stage-version" || completed != 1 {
					t.Fatal("valid completion", err)
				}
			} else if err == nil || completed != 0 {
				t.Fatal("invalid provider parts completed")
			}
		})
	}
}
func TestCOSMultipartUnknownInitiationIsNotAutomaticallyRetried(t *testing.T) {
	calls := 0
	s := cosFixture(t, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("response lost") })
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/video"
	if _, err := s.InitiateStagingMultipart(context.Background(), key); !errors.Is(err, ErrCOSMultipartUnknown) || calls != 1 {
		t.Fatalf("unknown initiation calls=%d err=%v", calls, err)
	}
}

func TestCOSMultipartListingPagination(t *testing.T) {
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/video"
	listed, completed := 0, 0
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			completed++
			return cosResponse(r, `<CompleteMultipartUploadResult><ETag>assembled</ETag></CompleteMultipartUploadResult>`, "version"), nil
		}
		listed++
		var body strings.Builder
		fmt.Fprintf(&body, `<ListPartsResult><Key>%s</Key><UploadId>upload</UploadId>`, key)
		switch r.URL.Query().Get("part-number-marker") {
		case "0":
			for number := 1; number <= 1000; number++ {
				fmt.Fprintf(&body, `<Part><PartNumber>%d</PartNumber><ETag>etag-%d</ETag><Size>%d</Size></Part>`, number, number, 8<<20)
			}
			body.WriteString(`<IsTruncated>true</IsTruncated><NextPartNumberMarker>1000</NextPartNumberMarker>`)
		case "1000":
			body.WriteString(`<Part><PartNumber>1001</PartNumber><ETag>last</ETag><Size>17</Size></Part>`)
		default:
			t.Fatal("invalid pagination marker")
		}
		body.WriteString(`</ListPartsResult>`)
		return cosResponse(r, body.String(), ""), nil
	})
	version, err := s.CompleteStagingMultipart(context.Background(), key, "upload", 1000*(8<<20)+17)
	if err != nil || version != "version" || listed != 2 || completed != 1 {
		t.Fatalf("pagination listed=%d completed=%d err=%v", listed, completed, err)
	}
}
func TestCOSMultipartInitiationAndAbortStayOnStagingKey(t *testing.T) {
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/video"
	calls := 0
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/"+key {
			t.Fatal("changed staging key")
		}
		switch r.Method {
		case http.MethodPost:
			return cosResponse(r, fmt.Sprintf(`<InitiateMultipartUploadResult><Key>%s</Key><UploadId>upload-id</UploadId></InitiateMultipartUploadResult>`, key), ""), nil
		case http.MethodDelete:
			if r.URL.Query().Get("uploadId") != "upload-id" {
				t.Fatal("wrong abort target")
			}
			return cosResponse(r, "", ""), nil
		default:
			t.Fatal("unexpected method")
			return nil, nil
		}
	})
	id, err := s.InitiateStagingMultipart(context.Background(), key)
	if err != nil || id != "upload-id" {
		t.Fatal(err)
	}
	if err := s.AbortStagingMultipart(context.Background(), key, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitiateStagingMultipart(context.Background(), strings.Replace(key, "/staging/", "/originals/", 1)); err == nil || calls != 2 {
		t.Fatal("archive scope accepted")
	}
}

func TestCOSMultipartCompletionUnknownOutcomeNeedsReconciliation(t *testing.T) {
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/video"
	for _, lost := range []bool{false, true} {
		calls := 0
		s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet {
				return cosResponse(r, fmt.Sprintf(`<ListPartsResult><Key>%s</Key><UploadId>upload</UploadId><Part><PartNumber>1</PartNumber><ETag>part</ETag><Size>17</Size></Part></ListPartsResult>`, key), ""), nil
			}
			calls++
			if lost {
				return nil, io.ErrUnexpectedEOF
			}
			return cosResponse(r, `<CompleteMultipartUploadResult><ETag>assembled</ETag></CompleteMultipartUploadResult>`, ""), nil
		})
		if _, err := s.CompleteStagingMultipart(context.Background(), key, "upload", 17); !errors.Is(err, ErrCOSMultipartUnknown) || calls != 1 {
			t.Fatalf("lost=%v completion calls=%d err=%v", lost, calls, err)
		}
	}
}

func TestCOSMultipartResumeAcceptsSparsePartsAndRejectsInvalidObservations(t *testing.T) {
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/video"
	for _, scenario := range []string{"sparse", "empty", "duplicate", "wrong-size", "out-of-range", "bad-marker"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" {
					t.Fatal("resume mutated provider")
				}
				marker := r.URL.Query().Get("part-number-marker")
				parts, tail := "", ""
				if scenario != "empty" {
					number, size := 1, int64(8<<20)
					if calls == 2 {
						if marker != "1" {
							t.Fatal("lost sparse pagination marker", marker)
						}
						number, size = 3, 17
						if scenario == "duplicate" {
							number = 1
							size = 8 << 20
						}
						if scenario == "wrong-size" {
							size = 18
						}
						if scenario == "out-of-range" {
							number = 4
						}
					} else {
						tail = `<IsTruncated>true</IsTruncated><NextPartNumberMarker>1</NextPartNumberMarker>`
						if scenario == "bad-marker" {
							tail = `<IsTruncated>true</IsTruncated><NextPartNumberMarker>2</NextPartNumberMarker>`
						}
					}
					parts = fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>observed</ETag><Size>%d</Size></Part>`, number, size)
				}
				return cosResponse(r, fmt.Sprintf(`<ListPartsResult><Key>%s</Key><UploadId>provider-upload</UploadId>%s%s</ListPartsResult>`, key, parts, tail), ""), nil
			})
			parts, err := s.ListStagingMultipartParts(context.Background(), key, "provider-upload", (16<<20)+17)
			switch scenario {
			case "sparse":
				if err != nil || len(parts) != 2 || parts[0].PartNumber != 1 || parts[1].PartNumber != 3 || parts[1].SizeBytes != 17 || calls != 2 {
					t.Fatal("sparse resume failed", parts, err)
				}
			case "empty":
				if err != nil || len(parts) != 0 || calls != 1 {
					t.Fatal("empty upload should resume from start", err)
				}
			default:
				if !errors.Is(err, ErrIncompleteBackup) {
					t.Fatal("invalid observations accepted", parts, err)
				}
			}
		})
	}
}
