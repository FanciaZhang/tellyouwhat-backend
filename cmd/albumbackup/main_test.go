package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/crc64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(r *http.Request, status int, data []byte, version string) *http.Response {
	h := http.Header{}
	h.Set("x-cos-version-id", version)
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}
}
func fixture(t *testing.T) (config, request, []byte) {
	t.Helper()
	data := []byte("Salted__synthetic encrypted fixture")
	p := filepath.Join(t.TempDir(), "bundle.enc")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return config{"backup-1250000000", "ap-guangzhou", "fixture-id", "fixture-key"}, request{Operation: "put", Path: p, Key: "albums/recovery/" + strings.Repeat("a", 32) + ".enc", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data))}, data
}
func TestVersionPinnedUploadAndFetch(t *testing.T) {
	c, r, data := fixture(t)
	calls := []string{}
	transport := roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" || req.URL.Host != "backup-1250000000.cos.ap-guangzhou.myqcloud.com" || req.Header.Get("Authorization") == "" {
			t.Fatal("wrong endpoint or unsigned request")
		}
		calls = append(calls, req.Method+" "+req.URL.RequestURI())
		if req.URL.Query().Has("versioning") {
			return response(req, 200, []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), ""), nil
		}
		if req.Method == "PUT" {
			got, _ := io.ReadAll(req.Body)
			if !bytes.Equal(got, data) {
				t.Fatal("wrong upload")
			}
			res := response(req, 200, nil, "version-1")
			res.Header.Set("x-cos-hash-crc64ecma", strconv.FormatUint(crc64.Checksum(got, crc64.MakeTable(crc64.ECMA)), 10))
			return res, nil
		}
		if req.Method != "GET" || req.URL.Query().Get("versionId") != "version-1" {
			t.Fatal("unversioned read")
		}
		return response(req, 200, data, "version-1"), nil
	})
	receipt, err := transfer(context.Background(), c, r, transport)
	if err != nil {
		t.Fatal(err, calls)
	}
	if receipt.Version != "version-1" || len(calls) != 3 {
		t.Fatal("missing roundtrip proof")
	}
	r.Operation = "get"
	r.Version = receipt.Version
	r.Bucket = c.Bucket
	r.Region = c.Region
	r.Path = filepath.Join(t.TempDir(), "download.enc")
	if _, err := transfer(context.Background(), c, r, transport); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(r.Path)
	if !bytes.Equal(got, data) {
		t.Fatal("wrong download")
	}
	if _, err := transfer(context.Background(), c, r, transport); err == nil {
		t.Fatal("overwrote existing file")
	}
}
func TestUploadRefusesUncertainOrUnverifiedPublication(t *testing.T) {
	for _, mode := range []string{"unversioned", "null-version", "read-corrupt", "wrong-version", "short-read", "long-read", "put-lost", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			c, r, data := fixture(t)
			puts := 0
			tr := roundTrip(func(req *http.Request) (*http.Response, error) {
				if req.URL.Query().Has("versioning") {
					state := "Enabled"
					if mode == "unversioned" {
						state = "Suspended"
					}
					return response(req, 200, []byte(`<VersioningConfiguration><Status>`+state+`</Status></VersioningConfiguration>`), ""), nil
				}
				if req.Method == "PUT" {
					puts++
					body, _ := io.ReadAll(req.Body)
					if mode == "put-lost" {
						return nil, errors.New("response lost")
					}
					version := "version-1"
					if mode == "null-version" {
						version = "null"
					}
					res := response(req, 200, nil, version)
					res.Header.Set("x-cos-hash-crc64ecma", strconv.FormatUint(crc64.Checksum(body, crc64.MakeTable(crc64.ECMA)), 10))
					return res, nil
				}
				version := "version-1"
				body := append([]byte(nil), data...)
				switch mode {
				case "read-corrupt":
					body[0] ^= 1
				case "wrong-version":
					version = "version-2"
				case "short-read":
					body = body[:len(body)-1]
				case "long-read":
					body = append(body, 1)
				case "redirect":
					res := response(req, 302, nil, version)
					res.Header.Set("Location", "https://untrusted.example/bundle")
					return res, nil
				}
				if req.URL.Host == "untrusted.example" {
					t.Fatal("followed redirect")
				}
				res := response(req, 200, body, version)
				res.ContentLength = int64(len(data))
				return res, nil
			})
			if _, err := transfer(context.Background(), c, r, tr); err == nil {
				t.Fatal("published without proof")
			}
			if puts > 1 {
				t.Fatal("retried uncertain put")
			}
		})
	}
}
func TestTransferBoundariesAndFailedDownloadCleanup(t *testing.T) {
	c, r, data := fixture(t)
	never := roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid request reached provider")
		return nil, nil
	})
	for _, change := range []func(*request){func(r *request) { r.Key = "albums/originals/file" }, func(r *request) { r.Key = "albums/recovery/../file" }, func(r *request) { r.Bytes = maximum + 1 }, func(r *request) { r.SHA256 = strings.Repeat("0", 64) }, func(r *request) { r.Operation = "delete" }, func(r *request) { r.Operation = "get"; r.Version = "null" }} {
		bad := r
		change(&bad)
		if _, err := transfer(context.Background(), c, bad, never); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	r.Operation = "get"
	r.Version = "version-1"
	r.Bucket = c.Bucket
	r.Region = c.Region
	r.Path = filepath.Join(t.TempDir(), "partial.enc")
	tr := roundTrip(func(req *http.Request) (*http.Response, error) {
		bad := append([]byte(nil), data...)
		bad[0] ^= 1
		return response(req, 200, bad, "version-1"), nil
	})
	if _, err := transfer(context.Background(), c, r, tr); err == nil {
		t.Fatal("corrupt download accepted")
	}
	if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
		t.Fatal("partial download retained")
	}
	r.Bucket = "different-1250000000"
	if _, err := transfer(context.Background(), c, r, never); err == nil {
		t.Fatal("wrong bucket accepted")
	}
}

func TestSignedReceiptTransferUsesSamePinnedVerification(t *testing.T) {
	c, r, data := fixture(t)
	r.Key = strings.TrimSuffix(r.Key, ".enc") + ".receipt.json"
	r.Bucket = c.Bucket
	r.Region = c.Region
	reads := 0
	tr := roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Has("versioning") {
			return response(req, 200, []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), ""), nil
		}
		if req.Method == "PUT" {
			body, _ := io.ReadAll(req.Body)
			res := response(req, 200, nil, "receipt-version")
			res.Header.Set("x-cos-hash-crc64ecma", strconv.FormatUint(crc64.Checksum(body, crc64.MakeTable(crc64.ECMA)), 10))
			return res, nil
		}
		reads++
		if req.URL.Query().Get("versionId") != "receipt-version" {
			t.Fatal("unversioned receipt read")
		}
		return response(req, 200, data, "receipt-version"), nil
	})
	if _, err := transfer(context.Background(), c, r, tr); err != nil || reads != 1 {
		t.Fatal("receipt not verified", err)
	}
	never := roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid receipt reached provider")
		return nil, nil
	})
	r.Bytes = 65537
	if _, err := transfer(context.Background(), c, r, never); err == nil {
		t.Fatal("oversized receipt accepted")
	}
	r.Bytes = int64(len(data))
	r.Bucket = "wrong-1250000000"
	if _, err := transfer(context.Background(), c, r, never); err == nil {
		t.Fatal("receipt uploaded to different bucket")
	}
}
