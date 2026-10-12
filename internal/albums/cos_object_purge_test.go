package albums

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

func TestCOSObjectPurgeExactNamespacesAndVersionIDs(t *testing.T) {
	owner, ref := uuid.NewString(), uuid.NewString()
	for _, key := range []string{
		"albums/originals/" + owner + "/" + ref + "/source",
		"albums/staging/" + owner + "/" + ref + "/source",
		"albums/derivatives/" + owner + "/" + ref + "/" + string(PhotoGridJPEG),
		"albums/video-previews/" + owner + "/" + ref + "/" + string(VideoDaily1080),
		"albums/live-pairs/" + owner + "/" + ref + "/motion",
		"albums/staging/live-pairs/" + owner + "/" + ref + "/photo",
	} {
		t.Run(strings.Split(key, "/")[1], func(t *testing.T) {
			calls := 0
			version := "opaque+&/=?#"
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "DELETE" || r.URL.Path != "/"+key || r.URL.Query().Get("versionId") != version || len(r.URL.Query()) != 1 {
					t.Fatal("unsafe deletion", r.URL.Path)
				}
				response := cosResponse(r, "", version)
				response.StatusCode = http.StatusNoContent
				return response, nil
			})
			if err := s.deletePurgeVersion(context.Background(), owner, purgeTarget{key}, DerivedObjectVersion{version, false, 1}); err != nil || calls != 1 {
				t.Fatal(err, calls)
			}
			if err := s.deletePurgeVersion(context.Background(), uuid.NewString(), purgeTarget{key}, DerivedObjectVersion{version, false, 1}); !errors.Is(err, ErrInvalidManifest) || calls != 1 {
				t.Fatal("foreign deletion", err)
			}
		})
	}
	for _, key := range []string{"albums/originals/" + owner + "/" + ref + "/../source", "albums/other/" + owner + "/" + ref + "/source", "albums/live-pairs/" + owner + "/" + ref + "/other"} {
		if (purgeTarget{key}).valid(owner) {
			t.Fatal("bad target accepted", key)
		}
	}
}

func TestCOSObjectPurgeVersionsBoundBatchesButNeverClaimIncompleteEmpty(t *testing.T) {
	owner := uuid.NewString()
	target := purgeTarget{"albums/originals/" + owner + "/" + uuid.NewString() + "/source"}
	for _, scenario := range []string{"large-history", "neighbor-pages", "foreign-bucket", "bad-version", "wrong-marker"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				q := r.URL.Query()
				if r.Method != "GET" || !q.Has("versions") || q.Get("prefix") != target.Key || q.Get("max-keys") != "1000" {
					t.Fatal("unbounded inventory")
				}
				result := cos.BucketGetObjectVersionsResult{Name: "albums-test-1250000000", Prefix: target.Key, KeyMarker: q.Get("key-marker"), VersionIdMarker: q.Get("version-id-marker")}
				switch scenario {
				case "large-history":
					count := 1000
					if calls == 2 {
						count = 1
					}
					if calls > 2 {
						count = 0
					}
					for i := range count {
						result.Version = append(result.Version, cos.ListVersionsResultVersion{Key: target.Key, VersionId: fmt.Sprintf("v-%d-%d", calls, i), Size: 1})
					}
					result.IsTruncated = calls == 1
				case "neighbor-pages":
					key := fmt.Sprintf("%s-neighbor-%d", target.Key, calls)
					result.Version = []cos.ListVersionsResultVersion{{Key: key, VersionId: "other", Size: 1}}
					result.IsTruncated = true
					result.NextKeyMarker = key
					result.NextVersionIdMarker = "other"
				case "foreign-bucket":
					result.Name = "foreign"
				case "bad-version":
					result.Version = []cos.ListVersionsResultVersion{{Key: target.Key, VersionId: "null", Size: 1}}
				case "wrong-marker":
					result.KeyMarker = "unrequested"
				}
				return videoResponse(t, r, result), nil
			})
			if scenario == "large-history" {
				for _, count := range []int{1000, 1, 0} {
					v, err := s.listPurgeVersions(context.Background(), owner, target)
					if err != nil || len(v) != count {
						t.Fatal("batch failed", len(v), err)
					}
				}
			} else {
				v, err := s.listPurgeVersions(context.Background(), owner, target)
				if !errors.Is(err, ErrConflict) || v != nil {
					t.Fatal("incomplete absence accepted", v, err)
				}
			}
			if scenario == "neighbor-pages" && calls != 5 {
				t.Fatal("unbounded pagination", calls)
			}
		})
	}
}

func TestCOSObjectPurgeMultipartExactInventoryAndIdempotentAbort(t *testing.T) {
	owner := uuid.NewString()
	target := purgeTarget{"albums/staging/" + owner + "/" + uuid.NewString() + "/source"}
	for _, scenario := range []string{"batch", "empty", "neighbor", "bad-bucket", "bad-marker", "duplicate", "truncated-empty", "bad-prefix"} {
		t.Run(scenario, func(t *testing.T) {
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				q := r.URL.Query()
				if r.Method != "GET" || !q.Has("uploads") || q.Get("prefix") != target.Key || q.Get("max-uploads") != "1000" {
					t.Fatal("wrong multipart inventory")
				}
				bucket, prefix, key, extra := "albums-test-1250000000", target.Key, target.Key, ""
				if scenario == "neighbor" {
					key += "-neighbor"
				}
				if scenario == "bad-bucket" {
					bucket = "foreign"
				}
				if scenario == "bad-prefix" {
					prefix = "foreign"
				}
				if scenario == "bad-marker" {
					extra = "<KeyMarker>unrequested</KeyMarker>"
				}
				if scenario == "truncated-empty" || scenario == "batch" {
					extra += "<IsTruncated>true</IsTruncated>"
				}
				entry := fmt.Sprintf("<Upload><Key>%s</Key><UploadId>multipart-one</UploadId></Upload>", key)
				if scenario == "duplicate" {
					entry += entry
				}
				if scenario == "empty" || scenario == "truncated-empty" {
					entry = ""
				}
				return cosResponse(r, fmt.Sprintf("<ListMultipartUploadsResult><Bucket>%s</Bucket><Prefix>%s</Prefix>%s%s</ListMultipartUploadsResult>", bucket, prefix, entry, extra), ""), nil
			})
			ids, err := s.listPurgeUploads(context.Background(), owner, target)
			if scenario == "batch" {
				if err != nil || len(ids) != 1 {
					t.Fatal(ids, err)
				}
			} else if scenario == "empty" || scenario == "neighbor" {
				if err != nil || len(ids) != 0 {
					t.Fatal(ids, err)
				}
			} else if !errors.Is(err, ErrConflict) || ids != nil {
				t.Fatal("unsafe multipart response", ids, err)
			}
		})
	}
	for _, code := range []string{"success", "NoSuchUpload", "AccessDenied", "NoSuchKey"} {
		t.Run(code, func(t *testing.T) {
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != "DELETE" || r.URL.Path != "/"+target.Key || r.URL.Query().Get("uploadId") != "multipart-one" || len(r.URL.Query()) != 1 {
					t.Fatal("wrong abort target")
				}
				response := cosResponse(r, `<Error><Code>`+code+`</Code></Error>`, "")
				response.StatusCode = 404
				if code == "success" {
					response.StatusCode = 204
				}
				return response, nil
			})
			err := s.abortPurgeUpload(context.Background(), owner, target, "multipart-one")
			if code == "success" || code == "NoSuchUpload" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrCOSOperation) {
				t.Fatal("unexpected error accepted", err)
			}
		})
	}
}
