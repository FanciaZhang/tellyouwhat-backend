package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

const recoveryPrefix = "albums/recovery/"

type inventoryVersion struct {
	Key, Version string
	Bytes        int64
	DeleteMarker bool
}
type inventoryUpload struct{ Key, UploadID string }
type inventoryResult struct {
	Bucket, Region, Prefix string
	Versions               []inventoryVersion
	Uploads                []inventoryUpload
}
type uploadPage struct {
	XMLName                                                                                 xml.Name `xml:"ListMultipartUploadsResult"`
	Bucket, Prefix, KeyMarker, UploadIdMarker, NextKeyMarker, NextUploadIdMarker, Delimiter string
	EncodingType                                                                            string
	EncodingTypeHyphen                                                                      string `xml:"Encoding-Type"`
	IsTruncated                                                                             *bool
	CommonPrefixes                                                                          []string `xml:"CommonPrefixes>Prefix"`
	Upload                                                                                  []struct{ Key, UploadId string }
}

func readInventoryPage(ctx context.Context, c config, query url.Values, page any, transport http.RoundTripper) error {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &cos.AuthorizationTransport{SecretID: c.SecretID, SecretKey: c.SecretKey, Transport: transport}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+c.Bucket+".cos."+c.Region+".myqcloud.com/?"+query.Encode(), nil)
	if err != nil {
		return rejected
	}
	resp, err := client.Do(req)
	if err != nil {
		return rejected
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || resp.StatusCode != 200 || len(data) > 4<<20 {
		return rejected
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var extra any
	if decoder.Decode(page) != nil || decoder.Decode(&extra) != io.EOF {
		return rejected
	}
	return nil
}

func listUploads(ctx context.Context, c config, prefix string, transport http.RoundTripper) ([]inventoryUpload, error) {
	if prefix != recoveryPrefix && !regexp.MustCompile(`^albums/recovery/[0-9a-f]{32}$`).MatchString(prefix) {
		return nil, rejected
	}
	marker, uploadMarker := "", ""
	seen := map[string]bool{}
	result := []inventoryUpload{}
	for range 10 {
		query := url.Values{"uploads": {""}, "prefix": {prefix}, "max-uploads": {"1000"}}
		if marker != "" {
			query.Set("key-marker", marker)
			query.Set("upload-id-marker", uploadMarker)
		}
		var page uploadPage
		if readInventoryPage(ctx, c, query, &page, transport) != nil || page.IsTruncated == nil || page.Bucket != c.Bucket || page.Prefix != prefix || page.KeyMarker != marker || page.UploadIdMarker != uploadMarker || page.EncodingType != "" || page.EncodingTypeHyphen != "" || page.Delimiter != "" || len(page.CommonPrefixes) != 0 || len(page.Upload) > 1000 {
			return nil, rejected
		}
		pageIDs := map[string]bool{}
		for _, u := range page.Upload {
			id := u.Key + "\x00" + u.UploadId
			if !strings.HasPrefix(u.Key, prefix) || !keyPattern.MatchString(u.Key) || !validVersion(u.UploadId) || seen[id] || len(result) >= 10000 {
				return nil, rejected
			}
			seen[id], pageIDs[id] = true, true
			result = append(result, inventoryUpload{u.Key, u.UploadId})
		}
		if !*page.IsTruncated {
			if page.NextKeyMarker != "" || page.NextUploadIdMarker != "" {
				return nil, rejected
			}
			return result, nil
		}
		if !pageIDs[page.NextKeyMarker+"\x00"+page.NextUploadIdMarker] || (page.NextKeyMarker == marker && page.NextUploadIdMarker == uploadMarker) {
			return nil, rejected
		}
		marker, uploadMarker = page.NextKeyMarker, page.NextUploadIdMarker
	}
	return nil, rejected
}

// This point-in-time inventory cannot prove that an earlier PUT will not finish later.
func auditInventory(ctx context.Context, c config, r request, transport http.RoundTripper) (inventoryResult, error) {
	out := inventoryResult{Bucket: c.Bucket, Region: c.Region, Prefix: recoveryPrefix, Versions: []inventoryVersion{}, Uploads: []inventoryUpload{}}
	if r != (request{Operation: "inventory", Key: recoveryPrefix, Bucket: c.Bucket, Region: c.Region}) {
		return out, rejected
	}
	if _, err := newClient(c, transport); err != nil {
		return out, err
	}
	versions, err := listVersions(ctx, c, recoveryPrefix, transport)
	if err != nil {
		return out, err
	}
	for _, v := range versions {
		out.Versions = append(out.Versions, inventoryVersion{v.key, v.version, v.size, v.marker})
	}
	out.Uploads, err = listUploads(ctx, c, recoveryPrefix, transport)
	return out, err
}
