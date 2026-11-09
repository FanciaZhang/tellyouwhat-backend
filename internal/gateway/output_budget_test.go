package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/capability"
	"github.com/tellyouwhat/backend/internal/contracts"
)

func TestHTTPJobAuthorizationPersistsAuthenticatedOutputBudget(t *testing.T) {
	server := newTestServer()
	jobService := &fakeJobService{}
	server.jobs = jobService
	server.dispatcher = &fakeDispatcher{}
	server.capabilities = capability.NewService([]byte("01234567890123456789012345678901"), capability.NewMemoryUseStore(), func() time.Time {
		return time.Date(2026, 8, 2, 8, 1, 0, 0, time.UTC)
	})
	issue := httptest.NewRecorder()
	server.Router().ServeHTTP(issue, authorizedRequest(http.MethodPost, "/v1/ai/job-capabilities", validBody()))
	if issue.Code != http.StatusCreated {
		t.Fatalf("capability issuance failed: status=%d body=%s", issue.Code, issue.Body.String())
	}
	var issued capability.Issued
	if err := json.Unmarshal(issue.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	enqueue := authorizedRequest(http.MethodPost, "/v1/ai/jobs", validBody())
	enqueue.Header.Set("X-Health-Job-Capability", issued.Token)
	enqueue.Header.Set("X-Health-Job-ID", issued.JobID)
	result := httptest.NewRecorder()
	server.Router().ServeHTTP(result, enqueue)
	if result.Code != http.StatusAccepted {
		t.Fatalf("job enqueue failed: status=%d body=%s", result.Code, result.Body.String())
	}
	if jobService.lastRequest.OutputBudget != contracts.DefaultOutputBudget() {
		t.Fatalf("signed server budget did not reach job persistence: %+v", jobService.lastRequest.OutputBudget)
	}
}
