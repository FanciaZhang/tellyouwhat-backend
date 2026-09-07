package gateway

import (
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/contracts"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fixedPolicyResolver struct{ err error }

func (r fixedPolicyResolver) Resolve(_ context.Context, req contracts.Request) (contracts.Request, error) {
	if r.err != nil {
		return contracts.Request{}, r.err
	}
	return req.WithExecutionPolicy(contracts.ExecutionPolicy{Version: "test-v1", Endpoint: "ep-published", ReasoningEffort: "low", TimeoutSeconds: 90})
}
func TestAIRequestUsesPublishedPolicyAfterAuthentication(t *testing.T) {
	for _, path := range []string{"/v1/ai/requests", "/v1/ai/streams"} {
		t.Run(path, func(t *testing.T) {
			server := newTestServer()
			provider := &fakeProvider{}
			server.provider = provider
			server.executionPolicies = fixedPolicyResolver{}
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, authorizedRequest(http.MethodPost, path, validBody()))
			if response.Code != 200 {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
			if provider.lastRequest.ExecutionPolicy == nil || provider.lastRequest.ExecutionPolicy.Endpoint != "ep-published" || provider.lastRequest.Options.ReasoningEffort != "low" {
				t.Fatal("provider missed published policy")
			}
		})
	}
}
func TestAIRequestDoesNotCallProviderWhenPolicyReadFails(t *testing.T) {
	server := newTestServer()
	provider := &fakeProvider{}
	server.provider = provider
	server.executionPolicies = fixedPolicyResolver{err: errors.New("offline")}
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/ai/requests", validBody()))
	if response.Code != 503 || provider.completeCalls != 0 {
		t.Fatal("bypassed unavailable published policy")
	}
}
