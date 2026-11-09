package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

type uploadReceipt struct{ Key, UploadID, Bucket, Region string }

const multipartProtocol = "cos-multipart-v1"

func validUploadID(id string) bool {
	return id != "null" && len(id) <= 1024 && regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(id)
}

// The caller must save this ID in its authenticated intent before sending parts.
func initiate(ctx context.Context, c config, r request, transport http.RoundTripper) (uploadReceipt, error) {
	out := uploadReceipt{Key: r.Key, Bucket: c.Bucket, Region: c.Region}
	if !keyPattern.MatchString(r.Key) || r != (request{Operation: "initiate", Key: r.Key, Bucket: c.Bucket, Region: c.Region}) {
		return out, rejected
	}
	client, err := newClient(c, transport)
	if err != nil {
		return out, err
	}
	if err = requireVersioning(ctx, client); err != nil {
		return out, err
	}
	value, resp, err := client.Object.InitiateMultipartUpload(ctx, r.Key, &cos.InitiateMultipartUploadOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{ContentType: "application/octet-stream", CacheControl: "no-store"}})
	closeResponse(resp)
	if err != nil || resp == nil || resp.StatusCode != 200 || value == nil || value.Key != r.Key || value.Bucket != c.Bucket || !validUploadID(value.UploadID) {
		return out, rejected
	}
	out.UploadID = value.UploadID
	return out, nil
}

func requireVersioning(ctx context.Context, client *cos.Client) error {
	state, resp, err := client.Bucket.GetVersioning(ctx)
	closeResponse(resp)
	if err != nil || state == nil || state.Status != "Enabled" {
		return rejected
	}
	return nil
}

func verifyObject(ctx context.Context, client *cos.Client, key, version, digest string, size int64) error {
	resp, err := client.Object.Get(ctx, key, nil, version)
	if err != nil || resp == nil {
		closeResponse(resp)
		return rejected
	}
	defer closeResponse(resp)
	h := sha256.New()
	n, readErr := io.Copy(h, io.LimitReader(resp.Body, size+1))
	if resp.StatusCode != 200 || resp.Header.Get("x-cos-version-id") != version || resp.ContentLength != size || readErr != nil || n != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return rejected
	}
	return nil
}

func multipartPut(ctx context.Context, client *cos.Client, c config, r request, file *os.File, transport http.RoundTripper) (string, error) {
	if err := requireVersioning(ctx, client); err != nil {
		return "", err
	}
	// A lost completion response can be recovered without creating another session.
	bundleKey := r.Key
	if strings.HasSuffix(bundleKey, ".receipt.json") {
		bundleKey = strings.TrimSuffix(bundleKey, ".receipt.json") + ".enc"
	}
	versions, err := inventory(ctx, c, bundleKey, transport)
	if err != nil {
		return "", err
	}
	version, total := "", int64(0)
	for _, v := range versions {
		if v.marker || v.key != r.Key {
			continue
		}
		total += v.size
		if v.size != r.Bytes || total > 4*maximum || verifyObject(ctx, client, r.Key, v.version, r.SHA256, r.Bytes) != nil {
			return "", rejected
		}
		version = v.version
	}
	if version != "" {
		return version, nil
	}
	parts := []cos.Object{}
	const partSize = int64(8 << 20)
	for offset, number := int64(0), 1; offset < r.Bytes; offset, number = offset+partSize, number+1 {
		size := min(partSize, r.Bytes-offset)
		resp, err := client.Object.UploadPart(ctx, r.Key, r.UploadID, number, io.NewSectionReader(file, offset, size), &cos.ObjectUploadPartOptions{ContentLength: size})
		if err != nil || resp == nil {
			closeResponse(resp)
			return "", rejected
		}
		etag := resp.Header.Get("ETag")
		closeResponse(resp)
		if resp.StatusCode != 200 || !regexp.MustCompile(`^(?:[0-9a-fA-F]{32}|"[0-9a-fA-F]{32}")$`).MatchString(etag) {
			return "", rejected
		}
		parts = append(parts, cos.Object{PartNumber: number, ETag: etag})
	}
	value, resp, err := client.Object.CompleteMultipartUpload(ctx, r.Key, r.UploadID, &cos.CompleteMultipartUploadOptions{Parts: parts})
	closeResponse(resp)
	if err != nil || value == nil || value.Key != r.Key || value.Bucket != c.Bucket || resp == nil || resp.StatusCode != 200 || !validVersion(resp.Header.Get("x-cos-version-id")) {
		return "", rejected
	}
	return resp.Header.Get("x-cos-version-id"), nil
}

func closeBackupUploads(ctx context.Context, client *cos.Client, c config, r request, transport http.RoundTripper) (int, error) {
	prefix := strings.TrimSuffix(r.Key, ".enc")
	targets := map[inventoryUpload]bool{}
	if r.UploadID != "" {
		targets[inventoryUpload{r.Key, r.UploadID}] = true
	}
	if r.ReceiptUploadID != "" {
		targets[inventoryUpload{prefix + ".receipt.json", r.ReceiptUploadID}] = true
	}
	uploads, err := listUploads(ctx, c, prefix, transport)
	if err != nil {
		return 0, err
	}
	for _, u := range uploads {
		if (u.Key != r.Key && u.Key != prefix+".receipt.json") || !validUploadID(u.UploadID) {
			return 0, rejected
		}
		targets[u] = true
	}
	closed := 0
	for u := range targets {
		resp, err := client.Object.AbortMultipartUpload(ctx, u.Key, u.UploadID)
		closeResponse(resp)
		var provider *cos.ErrorResponse
		if resp == nil || (err != nil && !(resp.StatusCode == 404 && errors.As(err, &provider) && provider.Code == "NoSuchUpload")) || (err == nil && resp.StatusCode != 204) {
			return closed, rejected
		}
		_, resp, err = client.Object.ListParts(ctx, u.Key, u.UploadID, nil)
		closeResponse(resp)
		provider = nil
		if resp == nil || resp.StatusCode != 404 || !errors.As(err, &provider) || provider.Code != "NoSuchUpload" {
			return closed, rejected
		}
		closed++
	}
	remaining, err := listUploads(ctx, c, prefix, transport)
	if err != nil || len(remaining) != 0 {
		return closed, rejected
	}
	return closed, nil
}
