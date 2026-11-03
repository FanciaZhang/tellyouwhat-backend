package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/platformops"
)

type operationsReader struct {
	p   platformops.Policy
	err error
}

func (r operationsReader) Current(context.Context) (platformops.Revision, error) {
	return platformops.Revision{Policy: r.p}, r.err
}
func TestPausedOperationsRejectBeforeProviderAndKeepOtherFunctionsAvailable(t *testing.T) {
	for _, path := range []string{"/v1/ai/requests", "/v1/ai/streams", "/v1/ai/job-capabilities"} {
		t.Run(path, func(t *testing.T) {
			s := newTestServer()
			s.jobs = &fakeJobService{}
			s.dispatcher = &fakeDispatcher{}
			s.capabilities = &fakeCapabilities{}
			model := &fakeProvider{}
			s.provider = model
			s.operations = operationsReader{p: platformops.Policy{Apps: map[string]platformops.AppPolicy{"health": {Paused: true}}}}
			response := httptest.NewRecorder()
			s.Router().ServeHTTP(response, authorizedRequest(http.MethodPost, path, validBody()))
			if response.Code != 503 || !strings.Contains(response.Body.String(), "ai_paused") || model.completeCalls != 0 {
				t.Fatalf("pause missed: %d %s", response.Code, response.Body.String())
			}
		})
	}
	s := newTestServer()
	s.operations = operationsReader{err: errors.New("offline")}
	response := httptest.NewRecorder()
	s.Router().ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/ai/requests", validBody()))
	if response.Code != 503 || !strings.Contains(response.Body.String(), "operations_unavailable") {
		t.Fatal("read failure did not close admission")
	}
}
