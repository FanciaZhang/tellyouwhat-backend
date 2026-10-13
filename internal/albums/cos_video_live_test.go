//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

// Explicit, paid test against the fixed development bucket. Use synthetic media
// only. Objects remain for inspection; this test never grants deletion rights.
func TestLiveCOSVideoAdapter(t *testing.T) {
	if os.Getenv("ALBUM_CI_ADAPTER_PROBE") != "1" {
		t.Skip("explicit live probe required")
	}
	directory := os.Getenv("ALBUM_CI_ADAPTER_DIRECTORY")
	payload, err := os.ReadFile(filepath.Join(directory, "source.mp4"))
	if err != nil || len(payload) < 16 || len(payload) > 16<<20 || string(payload[4:8]) != "ftyp" {
		t.Fatal("small synthetic MP4 required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("home unavailable")
	}
	var credentials [2]struct{ SecretId, SecretKey string }
	for i, name := range []string{"backend-cos.json", "upload-cos.json"} {
		data, err := os.ReadFile(filepath.Join(home, ".config/tellyouwhat/albums-test", name))
		if err != nil || json.Unmarshal(data, &credentials[i]) != nil {
			t.Fatal("credential file unavailable")
		}
	}
	postDiagnostic := map[string]any{}
	transport := cosTestTransport(func(request *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(request)
		if err == nil && response != nil && request.Method == "POST" && request.URL.Path == "/jobs" {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(body))
			postDiagnostic["status"] = response.StatusCode
			if readErr == nil {
				var failure struct{ Code, Message string }
				if response.StatusCode >= 400 && xml.Unmarshal(body, &failure) == nil {
					message := failure.Message
					for _, credential := range credentials {
						for _, value := range []string{credential.SecretId, credential.SecretKey} {
							if value != "" {
								message = strings.ReplaceAll(message, value, "[credential]")
							}
						}
					}
					message = regexp.MustCompile(`https?://[^\s<>"']+`).ReplaceAllString(message, "[URL]")
					postDiagnostic["code"], postDiagnostic["message"] = failure.Code, message
				}
				var accepted cos.CreateMediaJobsResult
				if response.StatusCode == 200 && xml.Unmarshal(body, &accepted) == nil && accepted.JobsDetail != nil {
					postDiagnostic["jobID"] = accepted.JobsDetail.JobId
				}
			}
		}
		return response, err
	})
	s, err := newCOSObjects(COSConfig{Bucket: "tellyouwhat-albums-1251995834", Region: "ap-guangzhou", SecretID: credentials[0].SecretId, SecretKey: credentials[0].SecretKey, UploadSecretID: credentials[1].SecretId, UploadSecretKey: credentials[1].SecretKey}, transport)
	if err != nil {
		t.Fatal("COS configuration invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.Check(ctx); err != nil {
		t.Fatal("bucket preflight failed")
	}
	j := videoJobFixture()
	j.ProviderJobID = ""
	j.Spec.QueueID = "p543daa336f7a4d4b896654b933f8881b"
	sum := sha256.Sum256(payload)
	j.Spec.Source.SizeBytes = int64(len(payload))
	j.Spec.Source.SHA256 = hex.EncodeToString(sum[:])
	r, err := s.client.Object.Put(ctx, j.Spec.Source.Key, bytes.NewReader(payload), nil)
	if err != nil || r == nil || r.StatusCode != 200 {
		t.Fatal("synthetic source upload failed")
	}
	j.Spec.Source.VersionID = r.Header.Get("x-cos-version-id")
	if r.Body != nil {
		r.Body.Close()
	}
	if !concreteCOSVersion(j.Spec.Source.VersionID) {
		t.Fatal("source version missing")
	}
	evidence := map[string]any{"source": j.Spec.Source, "submittedAt": j.SubmittedAt, "localJobID": j.ID, "post": postDiagnostic, "scope": "production COS video adapter; synthetic fixture; no database, account, app or deletion"}
	defer func() {
		evidence["passed"] = !t.Failed()
		data, e := json.MarshalIndent(evidence, "", "  ")
		if e != nil {
			t.Error("evidence encoding failed")
			return
		}
		if os.WriteFile(filepath.Join(directory, "adapter-evidence.json"), data, 0600) != nil {
			t.Error("evidence persistence failed")
		}
	}()
	profile, err := s.InspectVideoSource(ctx, j.Spec.Source)
	if err != nil {
		// Safe diagnostics contain only synthetic metadata, never signed URLs.
		info, response, callErr := s.client.CI.GetMediaInfo(ctx, j.Spec.Source.Key, nil, url.QueryEscape(j.Spec.Source.VersionID))
		evidence["mediaInfoRequestSucceeded"] = callErr == nil
		if response != nil {
			evidence["mediaInfoStatus"] = response.StatusCode
			evidence["mediaInfoVersion"] = response.Header.Get("x-cos-version-id")
		}
		if info != nil {
			evidence["mediaInfo"] = info.MediaInfo
		}
		t.Fatal("production source inspection failed")
	}
	evidence["sourceProfile"] = profile
	j.Spec.Profile = profile
	j.Spec.MaxBytes = 16 << 20
	j.SubmittedAt = time.Now().UTC()
	id, err := s.SubmitVideoOnce(ctx, j)
	if err != nil {
		evidence["stage"] = "submit"
		t.Fatal("production submission unknown/failed; no POST retry")
	}
	j.ProviderJobID = id
	evidence["jobID"] = id
	for {
		done, err := s.PollVideoSubmission(ctx, j)
		if err != nil {
			evidence["stage"] = "poll"
			t.Fatal("production polling failed")
		}
		if done {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("poll deadline")
		case <-time.After(3 * time.Second):
		}
	}
	found, err := s.FindVideoSubmission(ctx, j)
	if err != nil || found != id {
		evidence["stage"] = "find"
		t.Fatal("submission recovery failed")
	}
	result, err := s.VerifyVideoResult(ctx, j)
	if err != nil {
		evidence["stage"] = "verify"
		t.Fatal("production output verification failed")
	}
	evidence["result"] = result
	get, err := s.client.Object.Get(ctx, result.Object.Key, nil, url.QueryEscape(result.Object.VersionID))
	if err != nil || get == nil {
		t.Fatal("fixed output download failed")
	}
	output, err := io.ReadAll(io.LimitReader(get.Body, result.Object.SizeBytes+1))
	get.Body.Close()
	actual := sha256.Sum256(output)
	if err != nil || int64(len(output)) != result.Object.SizeBytes || hex.EncodeToString(actual[:]) != result.Object.SHA256 {
		t.Fatal("output bytes mismatch")
	}
	if os.WriteFile(filepath.Join(directory, "output.mp4"), output, 0600) != nil {
		t.Fatal("output save failed")
	}
	if err := s.checkVideoSource(ctx, j.Spec.Source); err != nil {
		t.Fatal("source latest version changed")
	}
	get, err = s.client.Object.Get(ctx, j.Spec.Source.Key, nil, url.QueryEscape(j.Spec.Source.VersionID))
	if err != nil || get == nil {
		t.Fatal("fixed source recheck failed")
	}
	actualSource, err := io.ReadAll(io.LimitReader(get.Body, int64(len(payload)+1)))
	get.Body.Close()
	if err != nil || !bytes.Equal(payload, actualSource) {
		t.Fatal("source bytes changed")
	}
	evidence["sourceLatestAndBytesUnchanged"] = true
}
