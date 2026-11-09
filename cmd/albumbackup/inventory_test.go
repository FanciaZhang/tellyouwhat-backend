package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

func TestRunInventoryEmitsOnlyCompleteMetadata(t *testing.T) {
	c, _, _ := fixture(t)
	for _, denied := range []bool{false, true} {
		func() {
			args, stdin, stdout, transport := os.Args, os.Stdin, os.Stdout, http.DefaultTransport
			defer func() { os.Args, os.Stdin, os.Stdout, http.DefaultTransport = args, stdin, stdout, transport }()
			folder := t.TempDir()
			configuration, _ := json.Marshal(c)
			if err := os.WriteFile(filepath.Join(folder, "config.json"), configuration, 0600); err != nil {
				t.Fatal(err)
			}
			input, err := os.CreateTemp(folder, "request-")
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			if err = json.NewEncoder(input).Encode(request{Operation: "inventory", Key: recoveryPrefix, Bucket: c.Bucket, Region: c.Region}); err != nil {
				t.Fatal(err)
			}
			if _, err = input.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			output, err := os.CreateTemp(folder, "output-")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			os.Args, os.Stdin, os.Stdout = []string{"albumbackup", filepath.Join(folder, "config.json")}, input, output
			http.DefaultTransport = roundTrip(func(req *http.Request) (*http.Response, error) {
				if req.Method != "GET" || req.URL.Path != "/" {
					t.Fatal("CLI mutated or read a body")
				}
				more := false
				if req.URL.Query().Has("versions") {
					return pageResponse(t, req, versionPage{Name: c.Bucket, Prefix: recoveryPrefix, IsTruncated: &more}), nil
				}
				if denied {
					return response(req, 403, nil, ""), nil
				}
				body, _ := xml.Marshal(uploadPage{Bucket: c.Bucket, Prefix: recoveryPrefix, IsTruncated: &more})
				return response(req, 200, body, ""), nil
			})
			err = run()
			data, readErr := os.ReadFile(output.Name())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if denied {
				if err == nil || len(data) != 0 {
					t.Fatal("partial CLI inventory emitted", err, string(data))
				}
				return
			}
			var result inventoryResult
			if err != nil || json.Unmarshal(data, &result) != nil || result.Versions == nil || result.Uploads == nil || result.Prefix != recoveryPrefix {
				t.Fatal("invalid CLI wire contract", err, string(data))
			}
		}()
	}
}

type cancelledInventoryBody struct {
	cancel context.CancelFunc
	closed bool
}

func (b *cancelledInventoryBody) Read([]byte) (int, error) { b.cancel(); return 0, context.Canceled }
func (b *cancelledInventoryBody) Close() error             { b.closed = true; return nil }

func TestAuditInventoryCancellationClosesBodyAndFails(t *testing.T) {
	c, _, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &cancelledInventoryBody{cancel: cancel}
	calls := 0
	_, err := auditInventory(ctx, c, request{Operation: "inventory", Key: recoveryPrefix, Bucket: c.Bucket, Region: c.Region}, roundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Query().Has("versions") {
			more := false
			return pageResponse(t, req, versionPage{Name: c.Bucket, Prefix: recoveryPrefix, IsTruncated: &more}), nil
		}
		resp := response(req, 200, nil, "")
		resp.Body = body
		return resp, nil
	}))
	if err == nil || !body.closed || ctx.Err() != context.Canceled || calls != 2 {
		t.Fatal("cancelled inventory was accepted or leaked response", err, body.closed, calls)
	}
}

func TestAuditInventoryReadsVersionsMarkersAndUploadsWithoutMutations(t *testing.T) {
	c, source, _ := fixture(t)
	other := recoveryPrefix + strings.Repeat("b", 32) + ".receipt.json"
	calls := 0
	tr := roundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		q := req.URL.Query()
		if req.Method != "GET" || req.URL.Path != "/" || req.URL.Host != c.Bucket+".cos."+c.Region+".myqcloud.com" || req.Header.Get("Authorization") == "" || q.Get("prefix") != recoveryPrefix || q.Has("delimiter") {
			t.Fatal("not a signed, namespace-only metadata read")
		}
		more := q.Get("key-marker") == ""
		if q.Has("versions") {
			if q.Get("max-keys") != "1000" {
				t.Fatal("unbounded versions")
			}
			p := versionPage{Name: c.Bucket, Prefix: recoveryPrefix, KeyMarker: q.Get("key-marker"), VersionIdMarker: q.Get("version-id-marker"), IsTruncated: &more}
			if more {
				p.Version = []cos.ListVersionsResultVersion{{Key: source.Key, VersionId: "v1", Size: source.Bytes}, {Key: source.Key, VersionId: "v2", Size: source.Bytes}}
				p.NextKeyMarker, p.NextVersionIdMarker = source.Key, "v2"
			} else {
				if p.KeyMarker != source.Key || p.VersionIdMarker != "v2" {
					t.Fatal("wrong version cursor")
				}
				p.DeleteMarker = []cos.ListVersionsResultDeleteMarker{{Key: other, VersionId: "deleted"}}
			}
			return pageResponse(t, req, p), nil
		}
		if !q.Has("uploads") || q.Get("max-uploads") != "1000" {
			t.Fatal("unexpected metadata operation")
		}
		p := uploadPage{Bucket: c.Bucket, Prefix: recoveryPrefix, KeyMarker: q.Get("key-marker"), UploadIdMarker: q.Get("upload-id-marker"), IsTruncated: &more}
		if more {
			p.Upload = append(p.Upload, struct{ Key, UploadId string }{source.Key, "upload1"})
			p.NextKeyMarker, p.NextUploadIdMarker = source.Key, "upload1"
		} else {
			if p.KeyMarker != source.Key || p.UploadIdMarker != "upload1" {
				t.Fatal("wrong upload cursor")
			}
			p.Upload = append(p.Upload, struct{ Key, UploadId string }{other, "upload2"})
		}
		data, err := xml.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return response(req, 200, data, ""), nil
	})
	out, err := auditInventory(context.Background(), c, request{Operation: "inventory", Key: recoveryPrefix, Bucket: c.Bucket, Region: c.Region}, tr)
	if err != nil || len(out.Versions) != 3 || len(out.Uploads) != 2 || !out.Versions[2].DeleteMarker || calls != 4 {
		t.Fatal(out, err, calls)
	}
	data, _ := json.Marshal(out)
	if !strings.Contains(string(data), `"DeleteMarker":true`) || !strings.Contains(string(data), `"UploadID":"upload2"`) {
		t.Fatal("wire contract omitted metadata", string(data))
	}
}

func TestAuditInventoryRejectsIncompleteUploadListing(t *testing.T) {
	for _, mode := range []string{"missing-truncation", "wrong-bucket", "wrong-prefix", "foreign-key", "null-id", "duplicate", "bad-pagination", "repeated-page", "page-limit", "common-prefix", "delimiter", "encoding", "encoding-hyphen", "marker-echo", "trailing-marker", "trailing-xml", "denied", "transport-error", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			c, source, _ := fixture(t)
			calls := 0
			tr := roundTrip(func(req *http.Request) (*http.Response, error) {
				if req.Method != "GET" || req.URL.Path != "/" {
					t.Fatal("audit performed a mutation or body read")
				}
				q := req.URL.Query()
				more := false
				if q.Has("versions") {
					return pageResponse(t, req, versionPage{Name: c.Bucket, Prefix: recoveryPrefix, IsTruncated: &more}), nil
				}
				calls++
				p := uploadPage{Bucket: c.Bucket, Prefix: recoveryPrefix, KeyMarker: q.Get("key-marker"), UploadIdMarker: q.Get("upload-id-marker"), IsTruncated: &more}
				p.Upload = append(p.Upload, struct{ Key, UploadId string }{source.Key, "upload"})
				switch mode {
				case "missing-truncation":
					p.IsTruncated = nil
				case "wrong-bucket":
					p.Bucket = "foreign-1250000000"
				case "wrong-prefix":
					p.Prefix = "albums/"
				case "foreign-key":
					p.Upload[0].Key = "health/private"
				case "null-id":
					p.Upload[0].UploadId = "null"
				case "duplicate":
					p.Upload = append(p.Upload, p.Upload[0])
				case "bad-pagination":
					more = true
					p.NextKeyMarker, p.NextUploadIdMarker = source.Key, "missing"
				case "repeated-page":
					more = true
					p.NextKeyMarker, p.NextUploadIdMarker = source.Key, "upload"
				case "page-limit":
					more = true
					p.Upload[0].UploadId = "upload" + strconv.Itoa(calls)
					p.NextKeyMarker, p.NextUploadIdMarker = source.Key, p.Upload[0].UploadId
				case "common-prefix":
					p.CommonPrefixes = []string{recoveryPrefix}
				case "delimiter":
					p.Delimiter = "/"
				case "encoding":
					p.EncodingType = "url"
				case "encoding-hyphen":
					p.EncodingTypeHyphen = "url"
				case "marker-echo":
					p.KeyMarker = source.Key
				case "trailing-marker":
					p.NextKeyMarker = source.Key
				case "denied":
					return response(req, 403, nil, ""), nil
				case "transport-error":
					return nil, errors.New("offline")
				case "oversized":
					return response(req, 200, []byte(strings.Repeat("x", (4<<20)+1)), ""), nil
				}
				body, _ := xml.Marshal(p)
				if mode == "trailing-xml" {
					body = append(body, []byte("<unexpected/>")...)
				}
				return response(req, 200, body, ""), nil
			})
			_, err := auditInventory(context.Background(), c, request{Operation: "inventory", Key: recoveryPrefix, Bucket: c.Bucket, Region: c.Region}, tr)
			if err == nil || calls > 10 {
				t.Fatal("incomplete listing accepted", calls)
			}
		})
	}
}

func TestAuditInventoryRequiresExactScopeBeforeNetwork(t *testing.T) {
	c, _, _ := fixture(t)
	base := request{Operation: "inventory", Key: recoveryPrefix, Bucket: c.Bucket, Region: c.Region}
	for _, change := range []func(*request){func(r *request) { r.Key = "albums/" }, func(r *request) { r.Bucket = "foreign-1250000000" }, func(r *request) { r.Path = "private" }, func(r *request) { r.Bytes = 1 }, func(r *request) { r.Version = "v1" }, func(r *request) { r.SHA256 = strings.Repeat("a", 64) }, func(r *request) { r.ReceiptBytes = 1 }} {
		r := base
		change(&r)
		_, err := auditInventory(context.Background(), c, r, roundTrip(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid scope reached network")
			return nil, rejected
		}))
		if err == nil {
			t.Fatal("invalid scope accepted")
		}
	}
}
