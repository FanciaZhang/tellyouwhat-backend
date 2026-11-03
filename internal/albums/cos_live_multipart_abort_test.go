//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

// Explicit probe: one synthetic session, no Complete, no existing media access.
// Abort removes only this probe's temporary parts. An intent marker forbids a
// second initialization after an uncertain outcome in the same evidence folder.
func TestLiveCOSLivePairMultipartAbortInFlight(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	owner, pair := uuid.NewString(), uuid.NewString()
	key := "albums/staging/live-pairs/" + owner + "/" + pair + "/photo"
	marker, err := os.OpenFile(filepath.Join(root, "multipart-intent.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("fresh evidence directory required")
	}
	err = json.NewEncoder(marker).Encode(map[string]string{"key": key, "owner": owner})
	closeErr := marker.Close()
	if err != nil || closeErr != nil {
		t.Fatal("intent could not be persisted")
	}
	id, err := s.InitiateStagingMultipart(ctx, key)
	if err != nil {
		t.Fatal("multipart initialization unknown; preserve intent")
	}
	roundtripJSON(t, root, "multipart-provider.json", map[string]string{"key": key, "owner": owner, "provider_id": id})
	data := bytes.Repeat([]byte("albums-multipart-abort-fixture-"), 1024)
	resource := Resource{ID: "photo", SizeBytes: int64(len(data)), SHA256: roundtripHash(data)}
	grant, err := s.AuthorizePart(ctx, key, id, resource, 1, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal("part grant unavailable")
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	makeRequest := func(call context.Context, body io.Reader) *http.Request {
		req, e := http.NewRequestWithContext(call, http.MethodPut, grant.URL, body)
		if e != nil {
			t.Fatal("request invalid")
		}
		req.ContentLength = resource.SizeBytes
		for k, v := range grant.Headers {
			req.Header.Set(k, v)
		}
		return req
	}
	response, err := client.Do(makeRequest(ctx, bytes.NewReader(data)))
	initialStatus := 0
	if response != nil {
		initialStatus = response.StatusCode
		response.Body.Close()
	}
	if err != nil || initialStatus != 200 {
		t.Fatal("probe part was not accepted")
	}
	parts, err := s.ListStagingMultipartParts(ctx, key, id, resource.SizeBytes)
	if err != nil || len(parts) != 1 || parts[0].SizeBytes != resource.SizeBytes {
		t.Fatal("uploaded part not observed")
	}
	call, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	prefix, release, writerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(writerDone)
		defer writer.Close()
		if _, e := writer.Write(data[:8192]); e != nil {
			return
		}
		close(prefix)
		select {
		case <-release:
		case <-call.Done():
			return
		}
		_, _ = writer.Write(data[8192:])
	}()
	type outcome struct {
		status int
		failed bool
		at     time.Time
	}
	done := make(chan outcome, 1)
	streamingRequest := makeRequest(call, reader)
	go func() {
		resp, e := client.Do(streamingRequest)
		o := outcome{failed: e != nil, at: time.Now().UTC()}
		if resp != nil {
			o.status = resp.StatusCode
			resp.Body.Close()
		}
		done <- o
	}()
	select {
	case <-prefix:
	case <-call.Done():
		t.Fatal("in-flight prefix not sent")
	}
	// Give COS time to receive headers while the remainder is intentionally held.
	timer := time.NewTimer(time.Second)
	select {
	case <-timer.C:
	case <-call.Done():
		timer.Stop()
		t.Fatal("probe cancelled")
	}
	abortAt := time.Now().UTC()
	abortErr := s.abortPurgeUpload(ctx, owner, purgeTarget{key}, id)
	abortedAt := time.Now().UTC()
	close(release)
	observed := <-done
	cancel()
	reader.Close()
	writer.Close()
	<-writerDone
	_, listed, listErr := s.client.Object.ListParts(ctx, key, id, nil)
	listStatus := 0
	if listed != nil {
		listStatus = listed.StatusCode
		if listed.Body != nil {
			listed.Body.Close()
		}
	}
	var providerError *cos.ErrorResponse
	absent := listStatus == 404 && errors.As(listErr, &providerError) && providerError.Code == "NoSuchUpload"
	repeatErr := s.abortPurgeUpload(ctx, owner, purgeTarget{key}, id)
	evidence := map[string]any{"key": key, "size_bytes": resource.SizeBytes, "initial_part_status": initialStatus, "initial_part_observed": true, "abort_started_at": abortAt, "abort_returned_at": abortedAt, "abort_confirmed": abortErr == nil, "inflight_response_at": observed.at, "inflight_response_status": observed.status, "inflight_transport_error": observed.failed, "session_missing_with_NoSuchUpload": absent, "repeat_abort_confirmed": repeatErr == nil, "complete_requests": 0, "existing_media_accessed": false, "object_versions_deleted": false}
	roundtripJSON(t, root, "multipart-abort-evidence.json", evidence)
	if abortErr != nil || !absent || repeatErr != nil || !observed.at.After(abortedAt) || observed.status >= 200 && observed.status < 300 {
		t.Fatal("multipart termination was not proved; inspect evidence")
	}
	t.Logf("initial=%d aborted=true in_flight=%d missing_session=true", initialStatus, observed.status)
}
