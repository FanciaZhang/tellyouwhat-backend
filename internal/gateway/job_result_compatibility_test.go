package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResultCapabilityRequiresClientOptInForClosedSchemaCompatibility(t *testing.T) {
	for _, preference := range []string{"", "download-v1", "unsupported"} {
		t.Run(preference, func(t *testing.T) {
			server := newTestServer()
			server.capabilities = &fakeCapabilities{}
			server.jobs = &fakeJobService{}
			server.dispatcher = &fakeDispatcher{}
			request := authorizedRequest(http.MethodPost, "/v1/ai/job-capabilities", validBody())
			if preference != "" {
				request.Header.Set("X-Health-Job-Result-Delivery", preference)
			}
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			if response.Code != 201 {
				t.Fatalf("capability: %d %s", response.Code, response.Body.String())
			}
			if preference == "download-v1" {
				var body map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body["resultToken"] != "valid-result-token" {
					t.Fatal("result delivery not enabled")
				}
			} else {
				var legacy struct {
					JobID     string    `json:"jobID"`
					Token     string    `json:"token"`
					ExpiresAt time.Time `json:"expiresAt"`
				}
				decoder := json.NewDecoder(strings.NewReader(response.Body.String()))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&legacy); err != nil {
					t.Fatalf("legacy closed-schema decoder failed: %v", err)
				}
				if legacy.JobID == "" || legacy.Token == "" || legacy.ExpiresAt.IsZero() {
					t.Fatal("legacy capability missing")
				}
			}
		})
	}
}
