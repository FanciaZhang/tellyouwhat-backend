//go:build ignore

// Run explicitly; creates only unique synthetic fixtures and never deletes objects.
package main

import (
	"bytes"
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
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

const bucket = "tellyouwhat-albums-1251995834"
const region = "ap-guangzhou"
const queue = "p543daa336f7a4d4b896654b933f8881b"

var safeCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func emit(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
func code(err error) string {
	var provider *cos.ErrorResponse
	if errors.As(err, &provider) && safeCode.MatchString(provider.Code) {
		return provider.Code
	}
	if err != nil {
		return "request_failed"
	}
	return ""
}
func status(r *cos.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}
func main() {
	if os.Getenv("ALBUM_CI_ROLE_PROBE") != "1" || len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "Explicit probe flag, synthetic fixture and output directory required")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		emit(map[string]any{"result": "failed", "stage": err.Error()})
		os.Exit(1)
	}
}
func run(fixture, directory string) error {
	payload, err := os.ReadFile(fixture)
	if err != nil || len(payload) < 32 || len(payload) > 1024*1024 || string(payload[4:8]) != "ftyp" {
		return errors.New("invalid_small_mp4_fixture")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return errors.New("home")
	}
	data, err := os.ReadFile(filepath.Join(home, ".config/tellyouwhat/albums-test/backend-cos.json"))
	if err != nil {
		return errors.New("credentials_missing")
	}
	var secret struct{ SecretId, SecretKey string }
	if json.Unmarshal(data, &secret) != nil || secret.SecretId == "" || secret.SecretKey == "" {
		return errors.New("credentials_invalid")
	}
	b, _ := url.Parse("https://" + bucket + ".cos." + region + ".myqcloud.com")
	c, _ := url.Parse("https://" + bucket + ".ci." + region + ".myqcloud.com")
	client := cos.NewClient(&cos.BaseURL{BucketURL: b, CIURL: c}, &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &cos.AuthorizationTransport{SecretID: secret.SecretId, SecretKey: secret.SecretKey}})
	client.Conf.RetryOpt.Count = 1
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	id := uuid.NewString()
	source := "albums/originals/ci-role-probe-" + id + "/source.mp4"
	staging := "albums/staging/ci-role-probe-" + id + "/source.mp4"
	base := "albums/video-previews/ci-role-probe-" + id + "/"
	versions := map[string]string{}
	for _, key := range []string{source, staging} {
		r, e := client.Object.Put(ctx, key, bytes.NewReader(payload), nil)
		if e != nil || r == nil || r.StatusCode != 200 {
			emit(map[string]any{"stage": "fixture_put", "status": status(r), "code": code(e)})
			return errors.New("fixture_put")
		}
		versions[key] = r.Header.Get("x-cos-version-id")
		if r.Body != nil {
			r.Body.Close()
		}
		if versions[key] == "" || versions[key] == "null" {
			return errors.New("fixture_version_missing")
		}
	}
	sum := sha256.Sum256(payload)
	emit(map[string]any{"fixture": source, "version": versions[source], "sizeBytes": len(payload), "sha256": hex.EncodeToString(sum[:]), "queue": queue})
	checks := []struct {
		name, input, output string
		deny                bool
	}{
		{"allowed_original_to_preview", source, base + "allowed.mp4", false},
		{"deny_staging_read", staging, base + "staging-denied.mp4", true},
		{"deny_original_write", source, "albums/originals/ci-role-probe-" + id + "/denied-output.mp4", true},
		{"deny_other_output_prefix", source, "albums/derivatives/ci-role-probe-" + id + "/denied-output.mp4", true},
	}
	for _, check := range checks {
		request := &cos.CreateMediaJobsOptions{Tag: "Transcode", QueueId: queue, CallBack: "no", Input: &cos.JobInput{Object: check.input}, Operation: &cos.MediaProcessJobOperation{
			Transcode: &cos.Transcode{Container: &cos.Container{Format: "mp4"}, Video: &cos.Video{Codec: "H.264", Width: "320", Height: "240", Fps: "24", Pixfmt: "yuv420p", Bitrate: "400"}, Audio: &cos.Audio{Codec: "aac", Bitrate: "64", Channels: "1"}},
			Output:    &cos.JobOutput{Bucket: bucket, Region: region, Object: check.output}, UserData: "albums-ci-role-probe:" + id + ":" + check.name}}
		result, r, e := client.CI.CreateMediaJobs(ctx, request)
		emit(map[string]any{"check": check.name, "phase": "submit", "status": status(r), "code": code(e)})
		if e != nil {
			if check.deny && status(r) == 403 && code(e) == "AccessDenied" {
				continue
			}
			return errors.New("submit_" + check.name)
		}
		if result == nil || result.JobsDetail == nil || !regexp.MustCompile(`^j[0-9a-f]{32}$`).MatchString(result.JobsDetail.JobId) {
			return errors.New("unknown_submission_no_retry")
		}
		detail := result.JobsDetail
		emit(map[string]any{"check": check.name, "jobID": detail.JobId})
		for detail.State != "Success" && detail.State != "Failed" && detail.State != "Cancel" {
			select {
			case <-ctx.Done():
				return errors.New("poll_timeout")
			case <-time.After(3 * time.Second):
			}
			result, r, e := client.CI.DescribeMediaJob(ctx, detail.JobId)
			if e != nil || r == nil || result == nil || result.JobsDetail == nil {
				return errors.New("poll_failed")
			}
			detail = result.JobsDetail
		}
		terminalCode := detail.Code
		if !safeCode.MatchString(terminalCode) {
			terminalCode = "unrecognized"
		}
		emit(map[string]any{"check": check.name, "phase": "terminal", "state": detail.State, "code": terminalCode, "jobID": detail.JobId})
		if check.deny {
			// Only an explicit authorization failure is evidence of isolation.
			if detail.State != "Failed" || !strings.Contains(strings.ToLower(detail.Code), "accessdenied") {
				return errors.New("not_access_denial_" + check.name)
			}
			continue
		}
		if detail.State != "Success" {
			return errors.New("positive_job_failed")
		}
		head, e := client.Object.Head(ctx, check.output, nil)
		if e != nil || head == nil || head.StatusCode != 200 {
			return errors.New("output_head")
		}
		version := head.Header.Get("x-cos-version-id")
		size := head.ContentLength
		if version == "" || version == "null" || size <= 0 || size > 1024*1024 {
			return errors.New("output_invalid")
		}
		read, e := client.Object.Get(ctx, check.output, nil, url.QueryEscape(version))
		if e != nil || read == nil {
			return errors.New("output_get")
		}
		output, e := io.ReadAll(io.LimitReader(read.Body, size+1))
		read.Body.Close()
		if e != nil || int64(len(output)) != size || read.Header.Get("x-cos-version-id") != version {
			return errors.New("output_version_or_size")
		}
		if os.WriteFile(filepath.Join(directory, "output.mp4"), output, 0600) != nil {
			return errors.New("output_local_write")
		}
		hash := sha256.Sum256(output)
		emit(map[string]any{"check": check.name, "output": check.output, "version": version, "sizeBytes": size, "sha256": hex.EncodeToString(hash[:])})
	}
	for key, version := range versions {
		head, e := client.Object.Head(ctx, key, nil)
		if e != nil || head == nil || head.Header.Get("x-cos-version-id") != version {
			return errors.New("source_latest_changed")
		}
		read, e := client.Object.Get(ctx, key, nil, url.QueryEscape(version))
		if e != nil || read == nil {
			return errors.New("source_read")
		}
		actual, e := io.ReadAll(io.LimitReader(read.Body, int64(len(payload)+1)))
		read.Body.Close()
		if e != nil || !bytes.Equal(actual, payload) {
			return errors.New("source_changed")
		}
	}
	emit(map[string]any{"result": "passed", "sourceLatestAndBytesUnchanged": true, "crossBucket": "not_tested", "automaticProcessingEnabled": false})
	return nil
}
