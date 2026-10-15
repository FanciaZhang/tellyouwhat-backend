//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Investigation only: create one tiny synthetic staging object and hold its
// request body across signature expiry. No user media or deletes are involved.
func TestLiveCOSPresignedUploadExpiryInFlight(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	data := bytes.Repeat([]byte("albums-upload-expiry-fixture-"), 1024)
	digest := sha256.Sum256(data)
	resource := Resource{ID: "photo", SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}
	key := stagingKey(Upload{OwnerID: uuid.NewString(), ID: uuid.NewString()}, resource.ID)
	expiry := time.Now().UTC().Add(8 * time.Second).Truncate(time.Second)
	grant, err := s.AuthorizePut(ctx, key, resource, expiry)
	if err != nil {
		t.Fatal("could not sign scoped probe upload")
	}
	call, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	var mu sync.Mutex
	var headersAt, prefixAt, tailAt time.Time
	call = httptrace.WithClientTrace(call, &httptrace.ClientTrace{WroteHeaders: func() { mu.Lock(); headersAt = time.Now().UTC(); mu.Unlock() }})
	request, err := http.NewRequestWithContext(call, http.MethodPut, grant.URL, reader)
	if err != nil {
		t.Fatal("could not construct probe")
	}
	request.ContentLength = resource.SizeBytes
	for k, v := range grant.Headers {
		request.Header.Set(k, v)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer writer.Close()
		if _, e := writer.Write(data[:8192]); e != nil {
			return
		}
		mu.Lock()
		prefixAt = time.Now().UTC()
		mu.Unlock()
		timer := time.NewTimer(time.Until(expiry.Add(3 * time.Second)))
		defer timer.Stop()
		select {
		case <-call.Done():
			writer.CloseWithError(call.Err())
			return
		case <-timer.C:
		}
		mu.Lock()
		tailAt = time.Now().UTC()
		mu.Unlock()
		_, _ = writer.Write(data[8192:])
	}()
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, sendErr := client.Do(request)
	finishedAt := time.Now().UTC()
	status := 0
	serverDate := ""
	if response != nil {
		status = response.StatusCode
		serverDate = response.Header.Get("Date")
		response.Body.Close()
	}
	cancel()
	reader.Close()
	writer.Close()
	<-done
	mu.Lock()
	sentHeaders, sentPrefix, sentTail := headersAt, prefixAt, tailAt
	mu.Unlock()
	if sentHeaders.IsZero() || !sentHeaders.Before(expiry) || sentPrefix.IsZero() {
		t.Fatal("probe did not start before expiry")
	}
	// Read only metadata and the exact tiny test object to distinguish a complete
	// upload from a failed request; no unrelated object is ever inspected.
	head, headErr := s.client.Object.Head(ctx, key, nil)
	version := ""
	stored := false
	if head != nil {
		if head.Body != nil {
			head.Body.Close()
		}
		version = head.Header.Get("x-cos-version-id")
		stored = headErr == nil && head.StatusCode == 200 && head.ContentLength == resource.SizeBytes && concreteCOSVersion(version)
	}
	exact := false
	if stored {
		result, e := s.client.Object.Get(ctx, key, nil, url.QueryEscape(version))
		if e == nil && result != nil && result.Body != nil {
			b, readErr := io.ReadAll(io.LimitReader(result.Body, resource.SizeBytes+1))
			result.Body.Close()
			exact = readErr == nil && bytes.Equal(b, data) && result.Header.Get("x-cos-version-id") == version
		}
	}
	if stored && !exact {
		t.Fatal("probe object bytes not confirmed")
	}
	result := map[string]any{"observed": true, "key": key, "size_bytes": resource.SizeBytes, "sha256": resource.SHA256, "signature_expiry": expiry, "headers_sent_at": sentHeaders, "prefix_written_at": sentPrefix, "tail_started_at": sentTail, "response_at": finishedAt, "response_status": status, "response_server_date": serverDate, "transport_error": sendErr != nil, "stored_complete_object": stored, "exact_bytes_match": exact, "version_id": version, "completed_after_expiry": status == 200 && stored && exact && sentTail.After(expiry), "cloud_objects_deleted": false}
	roundtripJSON(t, root, "upload-expiry-inflight-evidence.json", result)
	t.Logf("status=%d complete=%t tail_after_expiry=%t", status, stored && exact, sentTail.After(expiry))
}
