package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
