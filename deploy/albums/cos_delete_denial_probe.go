// Run explicitly with ALBUM_COS_PERMISSION_PROBE=1. Only a new synthetic object is touched.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

func main() {
	if os.Getenv("ALBUM_COS_PERMISSION_PROBE") != "1" {
		fmt.Fprintln(os.Stderr, "Live probe disabled; set ALBUM_COS_PERMISSION_PROBE=1 explicitly.")
		os.Exit(2)
	}
	if err := probe(); err != nil {
		// Provider errors may contain signed request URLs.
		fmt.Fprintln(os.Stderr, "Permission probe failed; no credentials or provider error body printed.")
		os.Exit(1)
	}
}

func probe() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	clients := make([]*cos.Client, 2)
	for i, name := range []string{"backend-cos.json", "upload-cos.json"} {
		data, err := os.ReadFile(filepath.Join(home, ".config/tellyouwhat/albums-test", name))
		if err != nil {
			return err
		}
		var secret struct{ SecretId, SecretKey string }
		if json.Unmarshal(data, &secret) != nil || secret.SecretId == "" || secret.SecretKey == "" {
			return errors.New("configuration")
		}
		u, _ := url.Parse("https://tellyouwhat-albums-1251995834.cos.ap-guangzhou.myqcloud.com")
		clients[i] = cos.NewClient(&cos.BaseURL{BucketURL: u}, &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &cos.AuthorizationTransport{SecretID: secret.SecretId, SecretKey: secret.SecretKey},
		})
		clients[i].Conf.RetryOpt.Count = 1
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := "albums/originals/permission-probe-" + hex.EncodeToString(nonce[:]) + "/probe.txt"
	payload := []byte("Disposable album COS deletion-permission probe.\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	put, err := clients[0].Object.Put(ctx, key, bytes.NewReader(payload), nil)
	if err != nil || put == nil || put.StatusCode != http.StatusOK {
		return errors.New("fixture write")
	}
	version := put.Header.Get("x-cos-version-id")
	if put.Body != nil {
		put.Body.Close()
	}
	if version == "" || version == "null" {
		return errors.New("fixture version")
	}

	type check struct {
		Name   string `json:"name"`
		Status int    `json:"status"`
		Code   string `json:"code"`
		Passed bool   `json:"passed"`
	}
	checks := []check{}
	passed := true
	for i, name := range []string{"backend_version_delete_denied", "upload_version_delete_denied", "backend_delete_marker_denied"} {
		query := url.Values{}
		if i < 2 {
			query.Set("versionId", version)
		}
		client := clients[0]
		if i == 1 {
			client = clients[1]
		}
		response, err := client.Object.Delete(ctx, key, &cos.ObjectDeleteOptions{XOptionQuery: &query})
		result := check{Name: name}
		if response != nil {
			result.Status = response.StatusCode
			if response.Body != nil {
				response.Body.Close()
			}
		}
		var provider *cos.ErrorResponse
		if errors.As(err, &provider) && provider.Code == "AccessDenied" {
			result.Code = "AccessDenied"
		}
		result.Passed = result.Status == http.StatusForbidden && result.Code == "AccessDenied"
		checks = append(checks, result)
		passed = passed && result.Passed
		if !result.Passed {
			break
		}
	}
	read, err := clients[0].Object.Get(ctx, key, nil, url.QueryEscape(version))
	if err != nil || read == nil {
		return errors.New("fixture read")
	}
	data, readErr := io.ReadAll(io.LimitReader(read.Body, int64(len(payload)+1)))
	read.Body.Close()
	intact := readErr == nil && read.StatusCode == http.StatusOK && read.Header.Get("x-cos-version-id") == version && bytes.Equal(data, payload)
	passed = passed && intact
	digest := sha256.Sum256(payload)
	json.NewEncoder(os.Stdout).Encode(map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339), "bucket": "tellyouwhat-albums-1251995834",
		"key": key, "version": version, "bytes": len(payload), "sha256": hex.EncodeToString(digest[:]),
		"checks": checks, "exactVersionIntact": intact, "passed": passed,
		"scope": "Only the newly created synthetic object; no permission changes or existing media touched.",
	})
	if !passed {
		return errors.New("unexpected permission result")
	}
	return nil
}
