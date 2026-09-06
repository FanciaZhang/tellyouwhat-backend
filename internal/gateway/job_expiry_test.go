package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/jobs"
)

func TestJobHTTPResultBecomesUnavailableAtExpiration(t *testing.T) {
	t.Parallel()
	expires := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	now := expires.Add(-time.Nanosecond)
	store := jobs.NewMemoryStore()
	job := jobs.Job{
		AppID: "health", ID: "19be2f9e-bd92-4699-b561-e3816092114c",
		RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", BodyDigest: "digest",
		OwnerKeyID: "valid-key", OwnerDeviceID: "device-1", Status: jobs.StatusSucceeded,
		Result: `{"choice":"synthetic-private-result"}`, CreatedAt: expires.Add(-24 * time.Hour),
		UpdatedAt: expires.Add(-time.Hour), ExpiresAt: expires,
	}
	if _, err := store.CreateOrGet(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	server := newTestServer()
	server.jobs = jobs.NewService(store, func() time.Time { return now })
	router := server.Router()
	before := httptest.NewRecorder()
	router.ServeHTTP(before, authorizedRequest(http.MethodGet, "/v1/ai/jobs/"+job.ID, ""))
	if before.Code != http.StatusOK || !strings.Contains(before.Body.String(), "synthetic-private-result") {
		t.Fatalf("unexpired job lookup did not return its result: status=%d", before.Code)
	}
	now = expires
	expired := httptest.NewRecorder()
	router.ServeHTTP(expired, authorizedRequest(http.MethodGet, "/v1/ai/jobs/"+job.ID, ""))
	if expired.Code != http.StatusNotFound || !strings.Contains(expired.Body.String(), "job_not_found") {
		t.Fatalf("expired job did not use the unavailable response: status=%d", expired.Code)
	}
	if strings.Contains(expired.Body.String(), "synthetic-private-result") {
		t.Fatal("expired response leaked result content")
	}
	retained, err := store.Get(context.Background(), job.ID)
	if err != nil || retained.Result != job.Result {
		t.Fatal("expiry gate test must retain the row awaiting physical cleanup")
	}
}
