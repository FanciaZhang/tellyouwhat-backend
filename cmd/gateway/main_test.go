package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tellyouwhat/backend/internal/gateway"
)

func TestReadinessWithWorkerChecksStorageAndWorker(t *testing.T) {
	t.Parallel()
	workerCalls := 0
	workerPath := ""
	worker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		workerCalls++
		workerPath = request.URL.Path
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer worker.Close()
	readiness, err := readinessWithWorker(
		gateway.ReadinessFunc(func(context.Context) error { return nil }),
		worker.URL+"/internal/jobs/process?secret=discarded",
		worker.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := readiness.Ready(context.Background()); err != nil || workerCalls != 1 || workerPath != "/healthz" {
		t.Fatalf("healthy worker was not included in readiness: calls=%d path=%s err=%v", workerCalls, workerPath, err)
	}
}

func TestReadinessWithWorkerStopsAfterStorageFailure(t *testing.T) {
	t.Parallel()
	workerCalls := 0
	worker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { workerCalls++ }))
	defer worker.Close()
	readiness, err := readinessWithWorker(
		gateway.ReadinessFunc(func(context.Context) error { return errors.New("storage unavailable") }),
		worker.URL+"/internal/jobs/process",
		worker.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := readiness.Ready(context.Background()); err == nil || workerCalls != 0 {
		t.Fatalf("storage failure did not short circuit readiness: calls=%d err=%v", workerCalls, err)
	}
}

func TestReadinessWithWorkerRejectsInvalidURL(t *testing.T) {
	t.Parallel()
	if _, err := readinessWithWorker(gateway.ReadinessFunc(func(context.Context) error { return nil }), "worker:8081/internal/jobs/process", nil); err == nil {
		t.Fatal("expected invalid worker URL")
	}
}
