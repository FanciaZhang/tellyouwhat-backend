package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

type backupVersion struct {
	key, version string
	size         int64
	marker       bool
}
type expiryResult struct {
	Key, Bucket, Region string
	VersionsRemoved     int
	Empty               bool
}
type versionPage struct {
	XMLName                                                                                               xml.Name `xml:"ListVersionsResult"`
	Name, Prefix, KeyMarker, VersionIdMarker, NextKeyMarker, NextVersionIdMarker, EncodingType, Delimiter string
	IsTruncated                                                                                           *bool
	CommonPrefixes                                                                                        []string `xml:"CommonPrefixes>Prefix"`
	Version                                                                                               []cos.ListVersionsResultVersion
	DeleteMarker                                                                                          []cos.ListVersionsResultDeleteMarker
}

func inventory(ctx context.Context, c config, key string, transport http.RoundTripper) ([]backupVersion, error) {
	prefix := strings.TrimSuffix(key, ".enc")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &cos.AuthorizationTransport{SecretID: c.SecretID, SecretKey: c.SecretKey, Transport: transport}}
	marker, versionMarker := "", ""
	seen := map[string]bool{}
	var result []backupVersion
	for range 10 {
		query := url.Values{"versions": {""}, "prefix": {prefix}, "max-keys": {"1000"}}
		if marker != "" {
			query.Set("key-marker", marker)
			query.Set("version-id-marker", versionMarker)
		}
		u := "https://" + c.Bucket + ".cos." + c.Region + ".myqcloud.com/?" + query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, rejected
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, rejected
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
		resp.Body.Close()
		if resp.StatusCode != 200 || readErr != nil || len(data) > 4<<20 {
			return nil, rejected
		}
		var page versionPage
		decoder := xml.NewDecoder(bytes.NewReader(data))
		var extra any
		if decoder.Decode(&page) != nil || decoder.Decode(&extra) != io.EOF || page.IsTruncated == nil || page.Name != c.Bucket || page.Prefix != prefix || page.KeyMarker != marker || page.VersionIdMarker != versionMarker || page.EncodingType != "" || page.Delimiter != "" || len(page.CommonPrefixes) != 0 || len(page.Version)+len(page.DeleteMarker) > 1000 {
			return nil, rejected
		}
		pageIDs := map[string]bool{}
		add := func(k, v string, size int64, deleted bool) bool {
			id := k + "\x00" + v
			if (k != key && k != prefix+".receipt.json") || !validVersion(v) || size < 0 || seen[id] || len(result) >= 10000 {
				return false
			}
			seen[id] = true
			pageIDs[id] = true
			result = append(result, backupVersion{k, v, size, deleted})
			return true
		}
		for _, v := range page.Version {
			if !add(v.Key, v.VersionId, v.Size, false) {
				return nil, rejected
			}
		}
		for _, v := range page.DeleteMarker {
			if !add(v.Key, v.VersionId, 0, true) {
				return nil, rejected
			}
		}
		if !*page.IsTruncated {
			if page.NextKeyMarker != "" || page.NextVersionIdMarker != "" {
				return nil, rejected
			}
			return result, nil
		}
		if !pageIDs[page.NextKeyMarker+"\x00"+page.NextVersionIdMarker] || (page.NextKeyMarker == marker && page.NextVersionIdMarker == versionMarker) {
			return nil, rejected
		}
		marker, versionMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
	return nil, rejected
}

// The Python producer authenticates expiry. This transport accepts only the two
// exact recovery keys and verifies every version before issuing any deletion.
func expire(ctx context.Context, c config, r request, transport http.RoundTripper) (expiryResult, error) {
	out := expiryResult{Key: r.Key, Bucket: c.Bucket, Region: c.Region}
	hashPattern := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if r.Operation != "expire" || !keyPattern.MatchString(r.Key) || !strings.HasSuffix(r.Key, ".enc") || r.Path != "" || r.Version != "" || r.Bucket != c.Bucket || r.Region != c.Region || r.Bytes <= 0 || r.Bytes > maximum || r.ReceiptBytes <= 0 || r.ReceiptBytes > 65536 || !hashPattern.MatchString(r.SHA256) || !hashPattern.MatchString(r.ReceiptSHA256) {
		return out, rejected
	}
	client, err := newClient(c, transport)
	if err != nil {
		return out, err
	}
	versions, err := inventory(ctx, c, r.Key, transport)
	if err != nil {
		return out, err
	}
	var verifyBytes int64
	for _, v := range versions {
		if v.marker {
			continue
		}
		size := r.Bytes
		if v.key != r.Key {
			size = r.ReceiptBytes
		}
		verifyBytes += size
		// shortcut: require an explicitly budgeted cleanup for more than 4 GiB of version verification.
		if v.size != size || verifyBytes > 4*maximum {
			return out, rejected
		}
	}
	for _, v := range versions {
		if v.marker {
			continue
		}
		size, digest := r.Bytes, r.SHA256
		if v.key != r.Key {
			size, digest = r.ReceiptBytes, r.ReceiptSHA256
		}
		resp, err := client.Object.Get(ctx, v.key, nil, v.version)
		if err != nil || resp == nil {
			closeResponse(resp)
			return out, rejected
		}
		hash := sha256.New()
		n, readErr := io.Copy(hash, io.LimitReader(resp.Body, size+1))
		closeResponse(resp)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("x-cos-version-id") != v.version || resp.ContentLength != size || readErr != nil || n != size || hex.EncodeToString(hash.Sum(nil)) != digest {
			return out, rejected
		}
	}
	for _, v := range versions {
		query := url.Values{"versionId": {v.version}}
		resp, err := client.Object.Delete(ctx, v.key, &cos.ObjectDeleteOptions{XOptionQuery: &query})
		closeResponse(resp)
		if err != nil || resp == nil || resp.StatusCode != http.StatusNoContent || (resp.Header.Get("x-cos-version-id") != "" && resp.Header.Get("x-cos-version-id") != v.version) {
			return out, rejected
		}
		out.VersionsRemoved++
	}
	remaining, err := inventory(ctx, c, r.Key, transport)
	if err != nil || len(remaining) != 0 {
		return out, rejected
	}
	out.Empty = true
	return out, nil
}
