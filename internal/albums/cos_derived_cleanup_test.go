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

func cleanupTargetFixture() DerivedCleanupTarget {
	return DerivedCleanupTarget{"video", uuid.NewString(), uuid.NewString(), string(VideoDaily1080)}
}
func cleanupListResponse(t *testing.T, r *http.Request, result cos.BucketGetObjectVersionsResult) *http.Response {
	t.Helper()
	return videoResponse(t, r, result)
}

func TestCOSDerivedVersionsBoundPaginationAndExcludePrefixNeighbours(t *testing.T) {
	target := cleanupTargetFixture()
	key, _ := target.key()
	calls := 0
	opaque := "opaque+&/=?#"
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/" || !q.Has("versions") || q.Get("prefix") != key || q.Get("max-keys") != "1000" {
			t.Fatal("unbounded or wrong listing")
		}
		result := cos.BucketGetObjectVersionsResult{Name: "albums-test-1250000000", Prefix: key, KeyMarker: q.Get("key-marker"), VersionIdMarker: q.Get("version-id-marker")}
		if calls == 1 {
			if q.Get("key-marker") != "" || q.Get("version-id-marker") != "" {
				t.Fatal("unexpected initial marker")
			}
			result.Version = []cos.ListVersionsResultVersion{{Key: key, VersionId: opaque, Size: 123}}
			result.IsTruncated = true
			result.NextKeyMarker = key
			result.NextVersionIdMarker = opaque
		} else {
			if calls != 2 || q.Get("key-marker") != key || q.Get("version-id-marker") != opaque {
				t.Fatal("opaque marker altered")
			}
			result.Version = []cos.ListVersionsResultVersion{{Key: key, VersionId: "v2", Size: 17}, {Key: key + "-neighbour", VersionId: "foreign", Size: 999}}
			result.DeleteMarker = []cos.ListVersionsResultDeleteMarker{{Key: key, VersionId: "marker"}}
		}
		return cleanupListResponse(t, r, result), nil
	})
	versions, err := s.ListDerivedVersions(context.Background(), target)
	if err != nil || calls != 2 || len(versions) != 3 || versions[0].VersionID != opaque || versions[2] != (DerivedObjectVersion{"marker", true, 0}) {
		t.Fatal("incomplete or unsafe listing", versions, err)
	}
}

func TestCOSDerivedVersionsRejectIncompleteForeignOrAmbiguousLists(t *testing.T) {
	for _, scenario := range []string{"bucket", "prefix", "key", "null-version", "duplicate", "encoding", "stalled", "page-cap"} {
		t.Run(scenario, func(t *testing.T) {
			target := cleanupTargetFixture()
			key, _ := target.key()
			calls := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				q := r.URL.Query()
				result := cos.BucketGetObjectVersionsResult{Name: "albums-test-1250000000", Prefix: key, KeyMarker: q.Get("key-marker"), VersionIdMarker: q.Get("version-id-marker"), Version: []cos.ListVersionsResultVersion{{Key: key, VersionId: fmt.Sprintf("v%d", calls), Size: 1}}}
				switch scenario {
				case "bucket":
					result.Name = "another-bucket"
				case "prefix":
					result.Prefix = "albums/originals/"
				case "key":
					result.Version[0].Key = "albums/originals/" + target.OwnerID + "/" + target.ReferenceID + "/movie"
				case "null-version":
					result.Version[0].VersionId = "null"
				case "duplicate":
					result.Version = append(result.Version, result.Version[0])
				case "encoding":
					result.EncodingType = "url"
				case "stalled":
					result.IsTruncated = true
					result.NextKeyMarker = key
					result.NextVersionIdMarker = "unchanging"
				case "page-cap":
					result.IsTruncated = true
					result.NextKeyMarker = key
					result.NextVersionIdMarker = fmt.Sprintf("next-%d", calls)
				}
				return cleanupListResponse(t, r, result), nil
			})
			if versions, err := s.ListDerivedVersions(context.Background(), target); !errors.Is(err, ErrConflict) || versions != nil || calls > 5 {
				t.Fatal("unsafe list accepted", versions, err, calls)
			}
		})
	}
}

func TestCOSDerivedDeleteRequiresExactVersionAndDerivedNamespace(t *testing.T) {
	target := cleanupTargetFixture()
	key, _ := target.key()
	version := "opaque+&/=?#"
	calls := 0
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodDelete || r.URL.Path != "/"+key || r.URL.Query().Get("versionId") != version || len(r.URL.Query()) != 1 {
			t.Fatal("versionless or wrong deletion")
		}
		response := cosResponse(r, "", version)
		response.StatusCode = http.StatusNoContent
		return response, nil
	})
	if err := s.DeleteDerivedVersion(context.Background(), target, DerivedObjectVersion{version, false, 3}); err != nil || calls != 1 {
		t.Fatal("fixed deletion failed", err, calls)
	}
	for _, invalid := range []DerivedCleanupTarget{{"originals", target.OwnerID, target.ReferenceID, "movie"}, {"video", target.OwnerID, target.ReferenceID, "../../originals"}, {"photo", target.OwnerID, target.ReferenceID, "source"}, {"video", target.OwnerID + "/../x", target.ReferenceID, string(VideoDaily1080)}} {
		if err := s.DeleteDerivedVersion(context.Background(), invalid, DerivedObjectVersion{version, false, 3}); !errors.Is(err, ErrInvalidManifest) {
			t.Fatal("invalid delete target accepted", err)
		}
	}
	for _, id := range []string{"", "null", "line\nbreak"} {
		if err := s.DeleteDerivedVersion(context.Background(), target, DerivedObjectVersion{id, false, 3}); !errors.Is(err, ErrInvalidManifest) {
			t.Fatal("nonconcrete version accepted", err)
		}
	}
	if calls != 1 {
		t.Fatal("invalid targets reached provider", calls)
	}
}

func TestCOSDerivedDeleteLostResponseHasNoImplicitRetry(t *testing.T) {
	target := cleanupTargetFixture()
	calls := 0
	s := cosFixture(t, func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("lost response with a private query")
	})
	if err := s.DeleteDerivedVersion(context.Background(), target, DerivedObjectVersion{"v1", false, 3}); !errors.Is(err, ErrCOSOperation) || calls != 1 || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe retry or provider error leaked", calls, err)
	}
}

func TestCOSDerivedDeleteSpecificMissingVersionAllowsReconciliation(t *testing.T) {
	for _, code := range []string{"NoSuchVersion", "NoSuchKey", "AccessDenied"} {
		t.Run(code, func(t *testing.T) {
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				response := cosResponse(r, "<Error><Code>"+code+"</Code><Message>private</Message></Error>", "")
				response.StatusCode = http.StatusNotFound
				return response, nil
			})
			err := s.DeleteDerivedVersion(context.Background(), cleanupTargetFixture(), DerivedObjectVersion{"v1", false, 3})
			if code == "AccessDenied" && !errors.Is(err, ErrCOSOperation) {
				t.Fatal("permission failure mistaken for deletion", err)
			}
			if code != "AccessDenied" && err != nil {
				t.Fatal("already deleted version not recoverable", err)
			}
		})
	}
}

func TestCOSDerivedPhotoAssessmentDistinguishesInvalidBytesFromUncertainty(t *testing.T) {
	spec, candidate := photoWriteFixture(t)
	key, _ := spec.key()
	p := PhotoWriteRecord{VersionID: uuid.NewString(), ManifestDigest: strings.Repeat("a", 64), Spec: spec}
	version := "output+&/?#"
	for _, scenario := range []string{"valid", "invalid", "network", "wrong-version", "short-body"} {
		t.Run(scenario, func(t *testing.T) {
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/"+key || r.URL.Query().Get("versionId") != version {
					t.Fatal("unfixed photo assessment")
				}
				if scenario == "network" {
					return nil, errors.New("network")
				}
				data := string(candidate.Bytes)
				if scenario == "invalid" {
					data = strings.Repeat("x", len(candidate.Bytes))
				}
				if scenario == "short-body" {
					data = data[:len(data)-1]
				}
				response := cosResponse(r, data, version)
				response.ContentLength = spec.SizeBytes
				if scenario == "wrong-version" {
					response.Header.Set("x-cos-version-id", "foreign")
				}
				return response, nil
			})
			receipt, invalid, err := s.AssessPhotoCleanup(context.Background(), p, []DerivedObjectVersion{{version, false, spec.SizeBytes}})
			switch scenario {
			case "valid":
				if err != nil || invalid || receipt == nil || receipt.VersionID != version {
					t.Fatal("valid photo not recovered", receipt, invalid, err)
				}
			case "invalid":
				if err != nil || !invalid || receipt != nil {
					t.Fatal("invalid committed photo not distinguished", receipt, invalid, err)
				}
			default:
				if !errors.Is(err, ErrCOSOperation) || invalid || receipt != nil {
					t.Fatal("uncertain read marked disposable", receipt, invalid, err)
				}
			}
		})
	}
	calls := 0
	s := cosFixture(t, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected GET") })
	if receipt, invalid, err := s.AssessPhotoCleanup(context.Background(), p, nil); err != nil || invalid || receipt != nil {
		t.Fatal("absence used as lost PUT proof", err)
	}
	if receipt, invalid, err := s.AssessPhotoCleanup(context.Background(), p, []DerivedObjectVersion{{version, false, spec.SizeBytes + 1}}); err != nil || !invalid || receipt != nil {
		t.Fatal("committed size mismatch ignored", err)
	}
	if _, invalid, err := s.AssessPhotoCleanup(context.Background(), p, []DerivedObjectVersion{{version, false, spec.SizeBytes}, {"other", false, spec.SizeBytes}}); !errors.Is(err, ErrConflict) || invalid {
		t.Fatal("ambiguous outputs retired", err)
	}
	if calls != 0 {
		t.Fatal("absence/ambiguous list issued GET", calls)
	}
}

func TestCOSDerivedVideoCleanupAcceptsOnlyBoundTerminalStates(t *testing.T) {
	for _, state := range []string{"Submitted", "Running", "Pause", "Success", "Failed", "Cancel", "Unknown", "foreign"} {
		t.Run(state, func(t *testing.T) {
			j := videoJobFixture()
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/jobs/"+j.ProviderJobID {
					t.Fatal("cleanup mutated provider task")
				}
				detail := videoDetail(j, state)
				if state == "foreign" {
					detail.Operation.UserData = "foreign"
					detail.State = "Failed"
				}
				return videoResponse(t, r, &cos.DescribeMediaProcessJobResult{JobsDetail: detail}), nil
			})
			observation, err := s.InspectVideoCleanup(context.Background(), j)
			switch state {
			case "Success":
				if err != nil || !observation.Terminal || !observation.Succeeded || observation.ProviderJobID != j.ProviderJobID {
					t.Fatal("success not bound", observation, err)
				}
			case "Failed", "Cancel":
				if err != nil || !observation.Terminal || observation.Succeeded {
					t.Fatal("terminal failure not bound", observation, err)
				}
			case "Unknown", "foreign":
				if err == nil || observation.Terminal {
					t.Fatal("unproved task marked terminal", observation, err)
				}
			default:
				if err != nil || observation.Terminal {
					t.Fatal("running task marked disposable", observation, err)
				}
			}
		})
	}
}
