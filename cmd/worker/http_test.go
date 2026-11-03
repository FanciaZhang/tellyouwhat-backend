package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/jobs"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/platformops"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorkerRouterUsesGeneratedContractAndAuthentication(t *testing.T) {
	t.Parallel()
	router := newWorkerRouter("worker-secret", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	health := httptest.NewRecorder()
	router.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("health = %d %s", health.Code, health.Body.String())
	}

	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/internal/jobs/process", strings.NewReader(`{`)))
	if unauthorized.Code != http.StatusUnauthorized || !strings.Contains(unauthorized.Body.String(), `"code":"unauthorized"`) {
		t.Fatalf("missing worker secret = %d %s", unauthorized.Code, unauthorized.Body.String())
	}

	invalid := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/internal/jobs/process", strings.NewReader(`{"appID":"health","jobID":"job"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Tellyouwhat-Worker-Secret", "worker-secret")
	router.ServeHTTP(invalid, request)
	if invalid.Code != http.StatusUnprocessableEntity || !strings.Contains(invalid.Body.String(), `"code":"invalid_job"`) {
		t.Fatalf("unknown job = %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestWorkerRouterEnforcesBodyLimitBeforeDecoding(t *testing.T) {
	t.Parallel()
	router := newWorkerRouter("worker-secret", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := append([]byte(`{"appID":"health","jobID":"`), bytes.Repeat([]byte("x"), int(workerRequestBodyLimit))...)
	body = append(body, []byte(`"}`)...)
	request := httptest.NewRequest(http.MethodPost, "/internal/jobs/process", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Tellyouwhat-Worker-Secret", "worker-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"payload_too_large"`) {
		t.Fatalf("oversized body = %d %s", response.Code, response.Body.String())
	}
}

func TestWorkerAdmissionDeferralSurvivesHTTPDispatch(t *testing.T) {
	ctx := context.Background()
	store := jobs.NewMemoryStore()
	id := "00000000-0000-4000-8000-000000000001"
	_, err := store.CreateOrGet(ctx, jobs.Job{ID: id, RequestID: id, Status: jobs.StatusQueued, ExpiresAt: time.Now().Add(time.Hour), Request: contracts.Request{Operation: contracts.OperationMealDecision}})
	if err != nil {
		t.Fatal(err)
	}
	worker := jobs.NewWorker(store, nil, nil)
	worker.Admit = func(context.Context, string) error { return platformops.ErrPaused }
	router := newWorkerRouter("fixture", map[appregistry.AppID]*jobs.Worker{appregistry.Health: worker}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(router)
	defer server.Close()
	dispatcher := jobs.NewHTTPDispatcher(server.URL+"/internal/jobs/process", "fixture", "health", server.Client())
	if err := dispatcher.Dispatch(ctx, id); !errors.Is(err, jobs.ErrAdmissionDeferred) {
		t.Fatalf("deferred protocol: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil || got.Status != jobs.StatusQueued || got.AttemptCount != 0 {
		t.Fatalf("queue: %+v %v", got, err)
	}
}
