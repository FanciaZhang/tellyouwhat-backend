package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/capability"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/healthhttpapi"
	"github.com/tellyouwhat/backend/internal/jobs"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
	"github.com/tellyouwhat/backend/internal/usage"
)

func resultFixture(t *testing.T) (*Server, *jobs.MemoryStore, capability.Issued, healthhttpapi.DownloadAIJobResultRequestObject) {
	t.Helper()
	server := newTestServer()
	service := capability.NewService([]byte("01234567890123456789012345678901"), capability.NewMemoryUseStore(), time.Now)
	principal := Principal{AppID: "health", KeyID: "valid-key", DeviceID: "device-1"}
	issued, err := service.Issue(principal, capability.Binding{RequestID: uuid.NewString(), Operation: contracts.OperationMealDecision, BodyDigest: "body", MediaDigest: "media"})
	if err != nil {
		t.Fatal(err)
	}
	store := jobs.NewMemoryStore()
	_, requestID, _ := service.ValidateResult(issued.ResultToken, issued.JobID)
	_, err = store.CreateOrGet(context.Background(), jobs.Job{AppID: "health", ID: issued.JobID, RequestID: requestID, BodyDigest: "body", OwnerKeyID: principal.KeyID, OwnerDeviceID: principal.DeviceID, Status: jobs.StatusQueued, CreatedAt: time.Now(), UpdatedAt: time.Now(), ExpiresAt: issued.ExpiresAt})
	if err != nil {
		t.Fatal(err)
	}
	server.jobs = jobs.NewService(store, time.Now)
	server.capabilities = service
	request := healthhttpapi.DownloadAIJobResultRequestObject{Id: uuid.MustParse(issued.JobID), Params: healthhttpapi.DownloadAIJobResultParams{XTellyouwhatRequestID: uuid.New(), XHealthJobResultCapability: issued.ResultToken}}
	return server, store, issued, request
}

func TestBackgroundResultWaitsForCompletionAndSupportsRepeatedHTTPDownload(t *testing.T) {
	server, store, issued, request := resultFixture(t)
	result := make(chan healthhttpapi.DownloadAIJobResultResponseObject, 1)
	go func() {
		value, _ := server.waitForJobResult(context.Background(), request, time.Second, time.Millisecond)
		result <- value
	}()
	select {
	case <-result:
		t.Fatal("returned before completion")
	case <-time.After(10 * time.Millisecond):
	}
	job, err := store.Claim(context.Background(), issued.JobID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Succeed(context.Background(), issued.JobID, job.AttemptCount, providerapi.Response{Content: `{"foods":[]}`}, usage.Record{RequestID: job.RequestID, KeyID: job.OwnerKeyID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-result:
		if value.(healthhttpapi.DownloadAIJobResult200JSONResponse).Status != "succeeded" {
			t.Fatal("not completed")
		}
	case <-time.After(time.Second):
		t.Fatal("did not deliver completion")
	}
	for i := 0; i < 2; i++ {
		httpRequest := httptest.NewRequest(http.MethodGet, "/v1/ai/jobs/"+issued.JobID+"/result", nil)
		httpRequest.Header.Set("X-Tellyouwhat-Request-ID", uuid.NewString())
		httpRequest.Header.Set("X-Health-Job-Result-Capability", issued.ResultToken)
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, httpRequest)
		if response.Code != 200 || !strings.Contains(response.Body.String(), "succeeded") || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("download failed: %d", response.Code)
		}
	}
}

func TestBackgroundResultBoundedWaitDoesNotResubmitAndCancellationStopsWaiting(t *testing.T) {
	server, store, issued, request := resultFixture(t)
	result, err := server.waitForJobResult(context.Background(), request, time.Millisecond, time.Millisecond)
	if err != nil || result.(healthhttpapi.DownloadAIJobResult200JSONResponse).Status != "queued" {
		t.Fatalf("bounded wait: %v", err)
	}
	job, _ := store.Get(context.Background(), issued.JobID)
	if job.AttemptCount != 0 {
		t.Fatal("result read started execution")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := server.waitForJobResult(ctx, request, time.Second, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if err := store.Cancel(context.Background(), issued.JobID, time.Now()); err != nil {
		t.Fatal(err)
	}
	result, err = server.waitForJobResult(context.Background(), request, time.Second, time.Millisecond)
	if err != nil || result.(healthhttpapi.DownloadAIJobResult200JSONResponse).Status != "cancelled" {
		t.Fatal("cancelled job not delivered")
	}
}

func TestBackgroundResultRejectsWrongJobAndUploadCapability(t *testing.T) {
	for _, wrongJob := range []bool{false, true} {
		server, _, issued, request := resultFixture(t)
		if wrongJob {
			request.Id = uuid.New()
		} else {
			request.Params.XHealthJobResultCapability = issued.Token
		}
		result, err := server.waitForJobResult(context.Background(), request, time.Second, time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		_ = result.VisitDownloadAIJobResultResponse(response)
		if response.Code != 401 {
			t.Fatalf("unauthorized read: %d", response.Code)
		}
	}
}
