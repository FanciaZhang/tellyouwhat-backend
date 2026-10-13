// albumbackup moves encrypted recovery bundles only; it never lists or deletes media.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

const maximum = int64(1 << 30) // shortcut: use multipart uploads if encrypted bundles exceed 1 GiB.
var rejected = errors.New("album backup transfer rejected")
var keyPattern = regexp.MustCompile(`^albums/recovery/[0-9a-f]{32}\.enc$`)
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9_.~-]+$`)

type config struct{ Bucket, Region, SecretID, SecretKey string }
type request struct {
	Operation, Path, Key, Version, SHA256, Bucket, Region string
	Bytes                                                 int64
}
type receipt struct {
	Key, Version, SHA256, Bucket, Region string
	Bytes                                int64
}

func privateFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, rejected
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, rejected
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.Close()
		return nil, rejected
	}
	return f, nil
}

func newClient(c config, transport http.RoundTripper) (*cos.Client, error) {
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,48}-[0-9]{5,20}$`).MatchString(c.Bucket) ||
		!regexp.MustCompile(`^[a-z]{2}-[a-z]+(?:-[a-z]+)?$`).MatchString(c.Region) || c.SecretID == "" || c.SecretKey == "" {
		return nil, rejected
	}
	endpoint, _ := url.Parse("https://" + c.Bucket + ".cos." + c.Region + ".myqcloud.com")
	client := cos.NewClient(&cos.BaseURL{BucketURL: endpoint}, &http.Client{
		Timeout:       10 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &cos.AuthorizationTransport{SecretID: c.SecretID, SecretKey: c.SecretKey, Transport: transport},
	})
	client.Conf.RetryOpt.Count = 1 // An uncertain PUT is not automatically reissued.
	return client, nil
}

func transfer(ctx context.Context, c config, r request, transport http.RoundTripper) (receipt, error) {
	out := receipt{Key: r.Key, Version: r.Version, SHA256: r.SHA256, Bytes: r.Bytes, Bucket: c.Bucket, Region: c.Region}
	if (r.Operation != "put" && r.Operation != "get") || !keyPattern.MatchString(r.Key) || r.Bytes <= 0 || r.Bytes > maximum || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(r.SHA256) ||
		(r.Operation == "put" && r.Version != "") || (r.Operation == "get" && (!validVersion(r.Version) || r.Bucket != c.Bucket || r.Region != c.Region)) {
		return receipt{}, rejected
	}
	client, err := newClient(c, transport)
	if err != nil {
		return receipt{}, err
	}
	if r.Operation == "put" {
		f, err := privateFile(r.Path)
		if err != nil {
			return receipt{}, err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(f, maximum+1))
		if err != nil || n != r.Bytes || hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
			return receipt{}, rejected
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return receipt{}, rejected
		}
		state, resp, err := client.Bucket.GetVersioning(ctx)
		closeResponse(resp)
		if err != nil || state == nil || state.Status != "Enabled" {
			return receipt{}, rejected
		}
		resp, err = client.Object.Put(ctx, r.Key, f, &cos.ObjectPutOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{ContentType: "application/octet-stream", CacheControl: "no-store", ContentLength: r.Bytes}})
		if resp != nil {
			out.Version = resp.Header.Get("x-cos-version-id")
		}
		closeResponse(resp)
		if err != nil || resp == nil || resp.StatusCode != http.StatusOK || !validVersion(out.Version) {
			return receipt{}, rejected
		}
	}
	// A successful PUT or an ETag is not enough: read and hash the exact version.
	resp, err := client.Object.Get(ctx, r.Key, nil, out.Version)
	if resp != nil {
		defer closeResponse(resp)
	}
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || resp.Header.Get("x-cos-version-id") != out.Version || resp.ContentLength != r.Bytes {
		return receipt{}, rejected
	}
	var destination *os.File
	var target io.Writer = io.Discard
	if r.Operation == "get" {
		destination, err = os.OpenFile(r.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return receipt{}, rejected
		}
		complete := false
		defer func() {
			destination.Close()
			if !complete {
				os.Remove(r.Path)
			}
		}()
		target = destination
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(target, h), io.LimitReader(resp.Body, r.Bytes+1))
		if copyErr != nil || n != r.Bytes || hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
			return receipt{}, rejected
		}
		if err = destination.Sync(); err != nil {
			return receipt{}, rejected
		}
		if err = destination.Close(); err != nil {
			return receipt{}, rejected
		}
		complete = true
	} else {
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(resp.Body, r.Bytes+1))
		if err != nil || n != r.Bytes || hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
			return receipt{}, rejected
		}
	}
	return out, nil
}
func validVersion(v string) bool {
	return v != "null" && len(v) <= 1024 && versionPattern.MatchString(v)
}
func closeResponse(r *cos.Response) {
	if r != nil && r.Body != nil {
		r.Body.Close()
	}
}

func run() error {
	if len(os.Args) != 2 {
		return rejected
	}
	f, err := privateFile(os.Args[1])
	if err != nil {
		return err
	}
	defer f.Close()
	var c config
	d := json.NewDecoder(io.LimitReader(f, 65537))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil {
		return rejected
	}
	var r request
	d = json.NewDecoder(io.LimitReader(os.Stdin, 65537))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil {
		return rejected
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	result, err := transfer(ctx, c, r, nil)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Albums encrypted backup transfer failed; no publication receipt issued.")
		os.Exit(1)
	}
}
