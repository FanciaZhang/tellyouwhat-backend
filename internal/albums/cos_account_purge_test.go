package albums

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func inventoryText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func inventoryXML(kind, prefix, entries string, truncated bool, nextKey, nextID string) string {
	root, bucket, limit, marker := "ListVersionsResult", "Name", "MaxKeys", "NextVersionIdMarker"
	if kind == "uploads" {
		root, bucket, limit, marker = "ListMultipartUploadsResult", "Bucket", "MaxUploads", "NextUploadIdMarker"
	}
	next := ""
	if nextKey != "" || nextID != "" {
		next = "<NextKeyMarker>" + inventoryText(nextKey) + "</NextKeyMarker><" + marker + ">" + inventoryText(nextID) + "</" + marker + ">"
	}
	return fmt.Sprintf("<%s><%s>albums-test-1250000000</%s><Prefix>%s</Prefix><%s>1000</%s><IsTruncated>%t</IsTruncated>%s%s</%s>", root, bucket, bucket, inventoryText(prefix), limit, limit, truncated, entries, next, root)
}

func inventoryEntry(kind, key, id string, deleted bool) string {
	if kind == "uploads" {
		return "<Upload><Key>" + inventoryText(key) + "</Key><UploadId>" + inventoryText(id) + "</UploadId></Upload>"
	}
	tag, extra := "Version", "<Size>12</Size>"
	if deleted {
		tag, extra = "DeleteMarker", ""
	}
	return "<" + tag + "><Key>" + inventoryText(key) + "</Key><VersionId>" + inventoryText(id) + "</VersionId>" + extra + "</" + tag + ">"
}

func accountInventoryKey(owner, area, reference string) string {
	prefix, _ := accountPurgePrefix(owner, area)
	part := "photo"
	if area == "derivatives" {
		part = string(PhotoGridJPEG)
	}
	if area == "video-previews" {
		part = string(VideoDaily1080)
	}
	return prefix + reference + "/" + part
}

func TestCOSAccountPurgeInventoryIncludesAllMediaNamespaces(t *testing.T) {
	owner := uuid.NewString()
	for _, area := range accountPurgeAreas {
		t.Run(area, func(t *testing.T) {
			prefix, _ := accountPurgePrefix(owner, area)
			key := accountInventoryKey(owner, area, uuid.NewString())
			otherKey := accountInventoryKey(owner, area, uuid.NewString())
			calls := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Scheme != "https" || r.URL.Host != "albums-test-1250000000.cos.ap-guangzhou.myqcloud.com" || r.Header.Get("Authorization") == "" {
					t.Fatal("unsigned or wrong-host inventory")
				}
				q := r.URL.Query()
				if r.Method == "DELETE" {
					if r.URL.Path != "/"+key || len(q) != 1 || q.Get("versionId") != "opaque+&/=?#" && q.Get("uploadId") != "upload-one" {
						t.Fatal("unsafe exact mutation")
					}
					response := cosResponse(r, "", q.Get("versionId"))
					response.StatusCode = 204
					return response, nil
				}
				if r.Method != "GET" || r.URL.Path != "/" || q.Get("prefix") != prefix || len(q) != 3 {
					t.Fatal("wrong account prefix request")
				}
				if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 30*time.Second {
					t.Fatal("unbounded inventory call")
				}
				kind := "versions"
				if q.Has("uploads") {
					kind = "uploads"
					if q.Get("max-uploads") != "1000" {
						t.Fatal("unbounded uploads")
					}
				} else if !q.Has("versions") || q.Get("max-keys") != "1000" {
					t.Fatal("unbounded versions")
				}
				entries := inventoryEntry(kind, key, "opaque+&/=?#", false) + inventoryEntry(kind, otherKey, "opaque+&/=?#", false)
				if kind == "versions" {
					entries += inventoryEntry(kind, key, "deleted", true)
				} else {
					entries = inventoryEntry(kind, key, "upload-one", false) + inventoryEntry(kind, otherKey, "upload-two", false)
				}
				return cosResponse(r, inventoryXML(kind, prefix, entries, false, "", ""), ""), nil
			})
			versions, err := s.listAccountPurgeVersions(context.Background(), owner, area)
			if err != nil || len(versions) != 3 || !versions[2].Version.DeleteMarker || versions[0].Target.Key != key || versions[1].Target.Key != otherKey {
				t.Fatal("incomplete versions", err)
			}
			if err = s.deletePurgeVersion(context.Background(), owner, versions[0].Target, versions[0].Version); err != nil {
				t.Fatal(err)
			}
			uploads, err := s.listAccountPurgeUploads(context.Background(), owner, area)
			if err != nil || len(uploads) != 2 || uploads[0].Target.Key != key {
				t.Fatal("missing orphan multipart", err)
			}
			if err = s.abortPurgeUpload(context.Background(), owner, uploads[0].Target, uploads[0].UploadID); err != nil {
				t.Fatal(err)
			}
			if calls != 4 {
				t.Fatal("unexpected requests", calls)
			}
		})
	}
}

func TestCOSAccountPurgeInventoryRejectsAmbiguousOrForeignResults(t *testing.T) {
	owner, area := uuid.NewString(), "staging"
	prefix, _ := accountPurgePrefix(owner, area)
	key := accountInventoryKey(owner, area, uuid.NewString())
	for _, kind := range []string{"versions", "uploads"} {
		for _, mode := range []string{"empty", "foreign-bucket", "foreign-prefix", "foreign-key", "prefix-neighbor", "unknown-key", "duplicate", "grouped", "delimiter", "encoding", "wrong-marker", "missing-truncation", "truncated-empty", "unseen-next", "empty-with-next", "bad-id", "oversize-page", "oversize-body", "wrong-root", "trailing-root", "access-denied", "redirect", "transport"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				calls := 0
				s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method != "GET" {
						t.Fatal("inventory issued mutation")
					}
					if mode == "transport" {
						return nil, errors.New("private storage error")
					}
					entryKey, id := key, "id-one"
					switch mode {
					case "foreign-key":
						entryKey = accountInventoryKey(uuid.NewString(), area, uuid.NewString())
					case "prefix-neighbor":
						entryKey = strings.Replace(key, owner, owner+"-neighbor", 1)
					case "unknown-key":
						entryKey = prefix + "../unknown"
					case "bad-id":
						id = ""
					}
					entry := inventoryEntry(kind, entryKey, id, false)
					if mode == "empty" || mode == "truncated-empty" || mode == "missing-truncation" || mode == "grouped" || mode == "delimiter" || mode == "empty-with-next" {
						entry = ""
					}
					if mode == "duplicate" {
						entry += entry
					}
					if mode == "oversize-page" {
						entry = strings.Repeat(entry, 1001)
					}
					nextKey, nextID := "", ""
					if mode == "unseen-next" || mode == "empty-with-next" {
						nextKey, nextID = key, "unseen"
					}
					body := inventoryXML(kind, prefix, entry, mode == "truncated-empty" || mode == "unseen-next", nextKey, nextID)
					switch mode {
					case "foreign-bucket":
						body = strings.Replace(body, "albums-test-1250000000", "foreign", 1)
					case "foreign-prefix":
						body = strings.Replace(body, inventoryText(prefix), "foreign-prefix", 1)
					case "grouped":
						body = strings.Replace(body, "<Prefix>", "<CommonPrefixes><Prefix>hidden/</Prefix></CommonPrefixes><Prefix>", 1)
					case "delimiter":
						body = strings.Replace(body, "<Prefix>", "<Delimiter>/</Delimiter><Prefix>", 1)
					case "encoding":
						tag := "EncodingType"
						if kind == "uploads" {
							tag = "Encoding-Type"
						}
						body = strings.Replace(body, "<Prefix>", "<"+tag+">url</"+tag+"><Prefix>", 1)
					case "wrong-marker":
						body = strings.Replace(body, "<Prefix>", "<KeyMarker>unrequested</KeyMarker><Prefix>", 1)
					case "missing-truncation":
						body = strings.Replace(body, "<IsTruncated>false</IsTruncated>", "", 1)
					case "oversize-body":
						body = strings.Repeat("x", (4<<20)+1)
					case "wrong-root":
						body = "<Wrong/>"
					case "trailing-root":
						body += "<Second/>"
					}
					response := cosResponse(r, body, "")
					if mode == "access-denied" {
						response.StatusCode = 403
					}
					if mode == "redirect" {
						response.StatusCode = 307
						response.Header.Set("Location", "https://foreign.example/secret")
					}
					return response, nil
				})
				var count int
				var err error
				if kind == "versions" {
					var v []accountPurgeVersion
					v, err = s.listAccountPurgeVersions(context.Background(), owner, area)
					count = len(v)
				} else {
					var v []accountPurgeUpload
					v, err = s.listAccountPurgeUploads(context.Background(), owner, area)
					count = len(v)
				}
				if mode == "empty" {
					if err != nil || count != 0 {
						t.Fatal("valid empty rejected", err)
					}
				} else if err == nil || count != 0 || strings.Contains(err.Error(), "private") {
					t.Fatal("unsafe inventory accepted", count, err)
				}
				if calls != 1 {
					t.Fatal("unexpected retry or redirect", calls)
				}
			})
		}
	}
}

func TestCOSAccountPurgeBatchesDrainBeyondPageLimitAndRelist(t *testing.T) {
	owner, area := uuid.NewString(), "originals"
	prefix, _ := accountPurgePrefix(owner, area)
	key := accountInventoryKey(owner, area, uuid.NewString())
	for _, kind := range []string{"versions", "uploads"} {
		t.Run(kind, func(t *testing.T) {
			var remaining []string
			for i := range 1003 {
				remaining = append(remaining, fmt.Sprintf("opaque-%04d", i))
			}
			lists, deletes := 0, 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "DELETE" {
					id := r.URL.Query().Get("versionId")
					if kind == "uploads" {
						id = r.URL.Query().Get("uploadId")
					}
					if r.URL.Path != "/"+key || len(r.URL.Query()) != 1 || len(remaining) == 0 || remaining[0] != id {
						t.Fatal("unexpected scoped deletion")
					}
					remaining = remaining[1:]
					deletes++
					response := cosResponse(r, "", r.URL.Query().Get("versionId"))
					response.StatusCode = 204
					return response, nil
				}
				lists++
				var entries strings.Builder
				count := min(len(remaining), 1000)
				for _, id := range remaining[:count] {
					entries.WriteString(inventoryEntry(kind, key, id, false))
				}
				truncated := len(remaining) > count
				nextKey, nextID := "", ""
				if truncated {
					nextKey, nextID = key, remaining[count-1]
				}
				return cosResponse(r, inventoryXML(kind, prefix, entries.String(), truncated, nextKey, nextID), ""), nil
			})
			for step := 0; step < 4; step++ {
				count := 0
				if kind == "versions" {
					batch, err := s.listAccountPurgeVersions(context.Background(), owner, area)
					if err != nil {
						t.Fatal(err)
					}
					count = len(batch)
					for _, v := range batch {
						if err = s.deletePurgeVersion(context.Background(), owner, v.Target, v.Version); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					batch, err := s.listAccountPurgeUploads(context.Background(), owner, area)
					if err != nil {
						t.Fatal(err)
					}
					count = len(batch)
					for _, u := range batch {
						if err = s.abortPurgeUpload(context.Background(), owner, u.Target, u.UploadID); err != nil {
							t.Fatal(err)
						}
					}
				}
				if count == 0 {
					break
				}
			}
			if len(remaining) != 0 || lists != 3 || deletes != 1003 {
				t.Fatal("incomplete multi-batch drain", len(remaining), lists, deletes)
			}
		})
	}
}

func TestCOSAccountPurgeRejectsCallerPrefixesBeforeNetwork(t *testing.T) {
	s := cosFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid scope reached provider")
		return nil, nil
	})
	for _, scope := range [][2]string{{"invalid", "originals"}, {uuid.NewString(), ""}, {uuid.NewString(), "../"}, {uuid.NewString(), "staging/" + uuid.NewString()}} {
		if _, err := s.listAccountPurgeVersions(context.Background(), scope[0], scope[1]); !errors.Is(err, ErrInvalidManifest) {
			t.Fatal(err)
		}
		if _, err := s.listAccountPurgeUploads(context.Background(), scope[0], scope[1]); !errors.Is(err, ErrInvalidManifest) {
			t.Fatal(err)
		}
	}
}

func TestCOSAccountPurgeVersionsAcceptKeyOnlyContinuation(t *testing.T) {
	owner := uuid.NewString()
	prefix, _ := accountPurgePrefix(owner, "originals")
	key := accountInventoryKey(owner, "originals", uuid.NewString())
	s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Fatal("inventory mutated storage")
		}
		body := inventoryXML("versions", prefix, inventoryEntry("versions", key, "version-one", false), true, key, "")
		return cosResponse(r, body, ""), nil
	})
	versions, err := s.listAccountPurgeVersions(context.Background(), owner, "originals")
	if err != nil || len(versions) != 1 {
		t.Fatal("documented key-only continuation rejected", err)
	}
}
