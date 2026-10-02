package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/gateway"
	"github.com/tellyouwhat/backend/internal/privacy"
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

func TestHealthRuntimeRecordsCurrentAndLegacyAgeConsent(t *testing.T) {
	t.Parallel()
	principal := gateway.Principal{AppID: "health", KeyID: "key", DeviceID: "device"}
	legacy := `{"consents":[{"scope":"adult","documentVersion":"2026-08-24","granted":true},{"scope":"privacy_and_terms","documentVersion":"2026-08-24","granted":true},{"scope":"sensitive_health_ai","documentVersion":"2026-08-24","granted":true}]}`
	current := `{"consents":[{"scope":"adult","documentVersion":"2026-08-24","granted":false},{"scope":"age_14_plus","documentVersion":"2026-10-01","granted":true},{"scope":"privacy_and_terms","documentVersion":"2026-10-01","granted":true},{"scope":"sensitive_health_ai","documentVersion":"2026-08-24","granted":true},{"scope":"managed_subscription","documentVersion":"2026-08-24","granted":true},{"scope":"lifetime_byok","documentVersion":"2026-08-24","granted":false},{"scope":"free_managed_recognition","documentVersion":"2026-08-24","granted":false}]}`
	for _, test := range []struct {
		name     string
		payload  string
		eligible bool
	}{
		{"legacy", legacy, true},
		{"current", current, true},
		{"revoked", strings.Replace(current, `"scope":"age_14_plus","documentVersion":"2026-10-01","granted":true`, `"scope":"age_14_plus","documentVersion":"2026-10-01","granted":false`, 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := privacy.NewService(privacy.NewMemoryRepository(), nil, nil, time.Now)
			dependencies := gateway.Dependencies{Authenticator: runtimeConsentAuthenticator{}, Privacy: service}
			configureHealthConsents(&dependencies)
			router := gateway.New(dependencies).Router()
			for _, payload := range []string{legacy, test.payload} {
				request := httptest.NewRequest(http.MethodPost, "/v1/privacy/consents", strings.NewReader(payload))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Tellyouwhat-Request-ID", "19be2f9e-bd92-4699-b561-e3816092114c")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("runtime rejected consent: %d %s", response.Code, response.Body.String())
				}
			}
			eligible, err := service.HasRequiredConsents(context.Background(), principal, dependencies.RequiredConsentScopes)
			if err != nil || eligible != test.eligible {
				t.Fatalf("eligibility = %v, error = %v, want %v", eligible, err, test.eligible)
			}
		})
	}
}

type runtimeConsentAuthenticator struct{}

func (runtimeConsentAuthenticator) Authenticate(context.Context, gateway.RequestProof) (gateway.Principal, error) {
	return gateway.Principal{AppID: "health", KeyID: "key", DeviceID: "device"}, nil
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
