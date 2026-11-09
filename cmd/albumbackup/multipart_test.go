package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"hash/crc64"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

func TestPrivateInputRetainsSharedLeaseUntilClosed(t *testing.T) {
	_, r, _ := fixture(t)
	f, err := privateFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	other, err := os.Open(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		t.Fatal("active input could be reclaimed")
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal("finished input stayed locked", err)
	}
}

func TestInitiationBindsSessionWithoutSendingContent(t *testing.T) {
	for _, mode := range []string{"ok", "lost", "wrong-key", "wrong-bucket", "null-id", "unversioned"} {
		t.Run(mode, func(t *testing.T) {
			c, source, _ := fixture(t)
			initiations := 0
			out, err := initiate(context.Background(), c, request{Operation: "initiate", Key: source.Key, Bucket: c.Bucket, Region: c.Region}, roundTrip(func(req *http.Request) (*http.Response, error) {
				if req.URL.Query().Has("versioning") {
					state := "Enabled"
					if mode == "unversioned" {
						state = "Suspended"
					}
					return response(req, 200, []byte(`<VersioningConfiguration><Status>`+state+`</Status></VersioningConfiguration>`), ""), nil
				}
				initiations++
				if req.Method != "POST" || !req.URL.Query().Has("uploads") || req.URL.Path != "/"+source.Key {
					t.Fatal("initiation sent content or wrong target")
				}
				if mode == "lost" {
					return nil, errors.New("lost initiation response")
				}
				key, bucket, id := source.Key, c.Bucket, "upload-id"
				if mode == "wrong-key" {
					key += "-other"
				}
				if mode == "wrong-bucket" {
					bucket = "other-1250000000"
				}
				if mode == "null-id" {
					id = "null"
				}
				return response(req, 200, []byte(`<InitiateMultipartUploadResult><Bucket>`+bucket+`</Bucket><Key>`+key+`</Key><UploadId>`+id+`</UploadId></InitiateMultipartUploadResult>`), ""), nil
			}))
			if mode == "ok" {
				if err != nil || out.UploadID != "upload-id" {
					t.Fatal(out, err)
				}
			} else if err == nil {
				t.Fatal("unsafe initiation accepted")
			}
			if initiations > 1 {
				t.Fatal("uncertain initiation retried")
			}
		})
	}
}

func TestMultipartUploadLostCompletionRecoversSamePinnedVersion(t *testing.T) {
	c, r, _ := fixture(t)
	data := bytes.Repeat([]byte("a"), (8<<20)+17)
	if err := os.WriteFile(r.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	r.SHA256 = hex.EncodeToString(digest[:])
	r.Bytes = int64(len(data))
	parts, completions, reads := 0, 0, 0
	stored := false
	tr := roundTrip(func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query()
		if q.Has("versioning") {
			return response(req, 200, []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), ""), nil
		}
		if q.Has("versions") {
			more := false
			p := versionPage{Name: c.Bucket, Prefix: strings.TrimSuffix(r.Key, ".enc"), IsTruncated: &more}
			if stored {
				p.Version = []cos.ListVersionsResultVersion{{Key: r.Key, VersionId: "committed", Size: r.Bytes}}
			}
			return pageResponse(t, req, p), nil
		}
		if q.Get("uploadId") != r.UploadID && req.Method != "GET" {
			t.Fatal("changed upload session")
		}
		if req.Method == "PUT" {
			parts++
			number, _ := strconv.Atoi(q.Get("partNumber"))
			offset := int64(number-1) * (8 << 20)
			body, _ := io.ReadAll(req.Body)
			if parts > 2 || number != parts || !bytes.Equal(body, data[offset:offset+int64(len(body))]) || int64(len(body)) != req.ContentLength {
				t.Fatal("wrong part geometry")
			}
			res := response(req, 200, nil, "")
			res.Header.Set("ETag", strings.Repeat(strconv.Itoa(number), 32))
			res.Header.Set("x-cos-hash-crc64ecma", strconv.FormatUint(crc64.Checksum(body, crc64.MakeTable(crc64.ECMA)), 10))
			return res, nil
		}
		if req.Method == "POST" {
			completions++
			if parts != 2 || completions != 1 {
				t.Fatal("blind completion retry")
			}
			var p cos.CompleteMultipartUploadOptions
			if xml.NewDecoder(req.Body).Decode(&p) != nil || len(p.Parts) != 2 || p.Parts[0].PartNumber != 1 || p.Parts[1].PartNumber != 2 {
				t.Fatal("wrong completion manifest")
			}
			stored = true
			return nil, errors.New("completion response lost after provider commit")
		}
		if req.Method != "GET" || q.Get("versionId") != "committed" {
			t.Fatal("unversioned recovery read")
		}
		reads++
		return response(req, 200, data, "committed"), nil
	})
	if _, err := transfer(context.Background(), c, r, tr); err == nil {
		t.Fatal("unknown outcome accepted")
	}
	out, err := transfer(context.Background(), c, r, tr)
	if err != nil || out.Version != "committed" || out.UploadID != r.UploadID || parts != 2 || completions != 1 || reads != 2 {
		t.Fatal(out, err, parts, completions, reads)
	}
}

func TestMultipartRefusesMissingSessionBadPartsAndEmbeddedCompletionError(t *testing.T) {
	for _, mode := range []string{"missing-session", "bad-etag", "part-failed", "embedded-error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			c, r, _ := fixture(t)
			if mode == "missing-session" {
				r.UploadID = ""
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			complete, network := 0, 0
			tr := roundTrip(func(req *http.Request) (*http.Response, error) {
				network++
				q := req.URL.Query()
				if q.Has("versioning") {
					return response(req, 200, []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), ""), nil
				}
				if q.Has("versions") {
					more := false
					return pageResponse(t, req, versionPage{Name: c.Bucket, Prefix: strings.TrimSuffix(r.Key, ".enc"), IsTruncated: &more}), nil
				}
				if req.Method == "PUT" {
					if mode == "cancel" {
						cancel()
						return nil, context.Canceled
					}
					if mode == "part-failed" {
						return response(req, 403, nil, ""), nil
					}
					resp := response(req, 200, nil, "")
					resp.Header.Set("ETag", "bad")
					if mode == "embedded-error" {
						resp.Header.Set("ETag", strings.Repeat("a", 32))
					}
					return resp, nil
				}
				complete++
				return response(req, 200, []byte(`<Error><Code>InternalError</Code><Message>not committed</Message></Error>`), ""), nil
			})
			if _, err := transfer(ctx, c, r, tr); err == nil {
				t.Fatal("unsafe multipart accepted")
			}
			if mode == "missing-session" && network != 0 || mode != "embedded-error" && complete != 0 || mode == "embedded-error" && complete != 1 {
				t.Fatal("unexpected write", network, complete)
			}
		})
	}
}

// Existing read/hash failure cases share only the successful multipart envelope.
func multipartResponses(t *testing.T, c config, r request, version string, next roundTrip) roundTrip {
	return roundTrip(func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query()
		if q.Has("versions") {
			more := false
			return pageResponse(t, req, versionPage{Name: c.Bucket, Prefix: strings.TrimSuffix(strings.TrimSuffix(r.Key, ".receipt.json"), ".enc"), IsTruncated: &more}), nil
		}
		if req.Method == "POST" && q.Has("uploadId") {
			if q.Get("uploadId") != r.UploadID || req.URL.Path != "/"+r.Key {
				t.Fatal("completion changed session")
			}
			var parts cos.CompleteMultipartUploadOptions
			if xml.NewDecoder(req.Body).Decode(&parts) != nil || len(parts.Parts) != 1 || parts.Parts[0].PartNumber != 1 {
				t.Fatal("incorrect completion manifest")
			}
			body := []byte(`<CompleteMultipartUploadResult><Bucket>` + c.Bucket + `</Bucket><Key>` + r.Key + `</Key><ETag>assembled</ETag></CompleteMultipartUploadResult>`)
			return response(req, 200, body, version), nil
		}
		if req.Method == "PUT" && (q.Get("uploadId") != r.UploadID || q.Get("partNumber") != "1") {
			t.Fatal("unmanaged PUT")
		}
		resp, err := next(req)
		if req.Method == "PUT" && resp != nil {
			resp.Header.Set("ETag", strings.Repeat("a", 32))
		}
		return resp, err
	})
}
