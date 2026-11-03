package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"testing"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

func expiryFixture(t *testing.T) (config, request, []byte, []byte) {
	c, r, data := fixture(t)
	receipt := []byte("controlled authenticated publication receipt")
	h := sha256.Sum256(receipt)
	r.Operation = "expire"
	r.Path = ""
	r.Bucket = c.Bucket
	r.Region = c.Region
	r.ReceiptSHA256 = hex.EncodeToString(h[:])
	r.ReceiptBytes = int64(len(receipt))
	return c, r, data, receipt
}
func pageResponse(t *testing.T, req *http.Request, p versionPage) *http.Response {
	t.Helper()
	body, err := xml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return response(req, 200, body, "")
}
func TestExpiryVerifiesAllPagesAndVersionsThenRechecksEmpty(t *testing.T) {
	c, r, data, receipt := expiryFixture(t)
	receiptKey := strings.TrimSuffix(r.Key, ".enc") + ".receipt.json"
	remaining := map[string]bool{"bundle-old": true, "bundle-new": true, "receipt": true, "marker": true}
	reads, deletes, lists := 0, 0, 0
	tr := roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "" || req.URL.Host != c.Bucket+".cos."+c.Region+".myqcloud.com" {
			t.Fatal("unsigned or wrong endpoint")
		}
		q := req.URL.Query()
		if q.Has("versions") {
			lists++
			more := false
			p := versionPage{Name: c.Bucket, Prefix: strings.TrimSuffix(r.Key, ".enc"), KeyMarker: q.Get("key-marker"), VersionIdMarker: q.Get("version-id-marker"), IsTruncated: &more}
			if len(remaining) > 0 {
				if p.KeyMarker == "" {
					more = true
					p.Version = []cos.ListVersionsResultVersion{{Key: r.Key, VersionId: "bundle-old", Size: r.Bytes}, {Key: r.Key, VersionId: "bundle-new", Size: r.Bytes}}
					p.NextKeyMarker = r.Key
					p.NextVersionIdMarker = "bundle-new"
				} else {
					if p.KeyMarker != r.Key || p.VersionIdMarker != "bundle-new" {
						t.Fatal("pagination mismatch")
					}
					p.Version = []cos.ListVersionsResultVersion{{Key: receiptKey, VersionId: "receipt", Size: r.ReceiptBytes}}
					p.DeleteMarker = []cos.ListVersionsResultDeleteMarker{{Key: receiptKey, VersionId: "marker"}}
				}
			}
			return pageResponse(t, req, p), nil
		}
		v := q.Get("versionId")
		if v == "" || !remaining[v] {
			t.Fatal("unversioned or wrong object operation")
		}
		if req.Method == "GET" {
			if deletes != 0 {
				t.Fatal("deleted before all verification completed")
			}
			reads++
			body := data
			if v == "receipt" {
				body = receipt
			}
			return response(req, 200, body, v), nil
		}
		if req.Method != "DELETE" || reads != 3 {
			t.Fatal("deletion before complete verification")
		}
		deletes++
		delete(remaining, v)
		return response(req, 204, nil, v), nil
	})
	out, err := expire(context.Background(), c, r, tr)
	if err != nil || !out.Empty || out.VersionsRemoved != 4 || lists != 3 || len(remaining) != 0 {
		t.Fatal(out, err, lists)
	}
	out, err = expire(context.Background(), c, r, tr)
	if err != nil || !out.Empty || out.VersionsRemoved != 0 {
		t.Fatal("replay was not empty", out, err)
	}
}
func TestExpiryRejectsIncompleteOrForeignInventoryBeforeDelete(t *testing.T) {
	for _, mode := range []string{"missing-truncation", "wrong-bucket", "wrong-prefix", "common-prefix", "foreign-key", "null-version", "duplicate-version", "bad-pagination", "repeated-page", "too-many-pages", "corrupt", "wrong-size", "wrong-read-version", "read-denied", "redirect", "trailing-xml"} {
		t.Run(mode, func(t *testing.T) {
			c, r, data, _ := expiryFixture(t)
			lists := 0
			tr := roundTrip(func(req *http.Request) (*http.Response, error) {
				if req.Method == "DELETE" {
					t.Fatal("unsafe delete")
				}
				q := req.URL.Query()
				if !q.Has("versions") {
					if mode == "read-denied" {
						return response(req, 403, nil, ""), nil
					}
					body := data
					version := "v1"
					if mode == "corrupt" {
						body = append([]byte(nil), data...)
						body[0] ^= 1
					}
					if mode == "wrong-read-version" {
						version = "other"
					}
					return response(req, 200, body, version), nil
				}
				lists++
				more := false
				p := versionPage{Name: c.Bucket, Prefix: strings.TrimSuffix(r.Key, ".enc"), KeyMarker: q.Get("key-marker"), VersionIdMarker: q.Get("version-id-marker"), IsTruncated: &more, Version: []cos.ListVersionsResultVersion{{Key: r.Key, VersionId: "v1", Size: r.Bytes}}}
				switch mode {
				case "missing-truncation":
					p.IsTruncated = nil
				case "wrong-bucket":
					p.Name = "other"
				case "wrong-prefix":
					p.Prefix = "albums/originals/"
				case "common-prefix":
					p.CommonPrefixes = []string{p.Prefix}
				case "foreign-key":
					p.Version[0].Key = r.Key + "-keep"
				case "null-version":
					p.Version[0].VersionId = "null"
				case "duplicate-version":
					p.Version = append(p.Version, p.Version[0])
				case "bad-pagination":
					more = true
					p.NextKeyMarker = r.Key
					p.NextVersionIdMarker = "not-listed"
				case "repeated-page":
					more = true
					p.NextKeyMarker = r.Key
					p.NextVersionIdMarker = "v1"
				case "too-many-pages":
					more = true
					p.Version[0].VersionId = strings.Repeat("v", lists)
					p.NextKeyMarker = r.Key
					p.NextVersionIdMarker = p.Version[0].VersionId
				case "wrong-size":
					p.Version[0].Size++
				case "redirect":
					res := response(req, 302, nil, "")
					res.Header.Set("Location", "https://untrusted.invalid")
					return res, nil
				case "trailing-xml":
					return response(req, 200, []byte(`<ListVersionsResult></ListVersionsResult><extra/>`), ""), nil
				}
				return pageResponse(t, req, p), nil
			})
			if _, err := expire(context.Background(), c, r, tr); err == nil {
				t.Fatal("unsafe expiry accepted")
			}
		})
	}
}
func TestExpiryLostDeleteAndConcurrentVersionNeverClaimCompletion(t *testing.T) {
	for _, mode := range []string{"lost-delete", "late-version", "list-failed"} {
		t.Run(mode, func(t *testing.T) {
			c, r, data, _ := expiryFixture(t)
			exists := true
			deletes := 0
			failed := false
			tr := roundTrip(func(req *http.Request) (*http.Response, error) {
				if req.URL.Query().Has("versions") {
					if deletes > 0 && mode == "list-failed" && !failed {
						failed = true
						return response(req, 403, nil, ""), nil
					}
					more := false
					p := versionPage{Name: c.Bucket, Prefix: strings.TrimSuffix(r.Key, ".enc"), IsTruncated: &more}
					if exists || deletes > 0 && mode == "late-version" {
						p.Version = []cos.ListVersionsResultVersion{{Key: r.Key, VersionId: "v1", Size: r.Bytes}}
					}
					return pageResponse(t, req, p), nil
				}
				if req.Method == "GET" {
					return response(req, 200, data, "v1"), nil
				}
				if req.Method != "DELETE" || req.URL.Query().Get("versionId") != "v1" {
					t.Fatal("unversioned deletion")
				}
				deletes++
				exists = false
				if mode == "lost-delete" {
					return nil, errors.New("response lost")
				}
				return response(req, 204, nil, "v1"), nil
			})
			if _, err := expire(context.Background(), c, r, tr); err == nil {
				t.Fatal("uncertain cleanup claimed completion")
			}
			if deletes != 1 {
				t.Fatal("retried uncertain delete", deletes)
			}
			if mode != "late-version" {
				out, err := expire(context.Background(), c, r, tr)
				if err != nil || !out.Empty || out.VersionsRemoved != 0 {
					t.Fatal("could not resume", out, err)
				}
			}
		})
	}
}
func TestExpiryRejectsInvalidScopeBeforeNetwork(t *testing.T) {
	c, r, _, _ := expiryFixture(t)
	never := roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid expiry reached provider")
		return nil, nil
	})
	for _, change := range []func(*request){func(r *request) { r.Key = "albums/originals/media" }, func(r *request) { r.Bucket = "wrong-1250000000" }, func(r *request) { r.ReceiptSHA256 = "" }, func(r *request) { r.ReceiptBytes = 65537 }, func(r *request) { r.Version = "null" }, func(r *request) { r.Path = "/user/file" }, func(r *request) { r.Bytes = maximum + 1 }} {
		bad := r
		change(&bad)
		if _, err := expire(context.Background(), c, bad, never); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
}

func TestExpiryBoundsVerificationTrafficBeforeReadingObjects(t *testing.T) {
	c, r, _, _ := expiryFixture(t)
	r.Bytes = maximum
	tr := roundTrip(func(req *http.Request) (*http.Response, error) {
		if !req.URL.Query().Has("versions") {
			t.Fatal("oversized cleanup read or deleted an object")
		}
		more := false
		p := versionPage{Name: c.Bucket, Prefix: strings.TrimSuffix(r.Key, ".enc"), IsTruncated: &more}
		for i := range 5 {
			p.Version = append(p.Version, cos.ListVersionsResultVersion{Key: r.Key, VersionId: strings.Repeat("v", i+1), Size: maximum})
		}
		return pageResponse(t, req, p), nil
	})
	if _, err := expire(context.Background(), c, r, tr); err == nil {
		t.Fatal("verification exceeded traffic bound")
	}
}
