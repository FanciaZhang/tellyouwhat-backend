package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/provider/ark"
)

func TestProviderCompletionControlsGatewaySuccessAndQuota(t *testing.T) {
	delta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"{}\"}\n\n"
	completed := `{"type":"response.completed","response":{"status":"completed","output_text":"{}","usage":{"input_tokens":12,"output_tokens":5}}}`
	for _, test := range []struct {
		name       string
		stream     bool
		body       string
		wantResult bool
		wantActual int
	}{
		{"stream_eof_after_valid_json", true, delta, false, 0},
		{"stream_done_without_completion", true, delta + "data: [DONE]\n\n", false, 0},
		{"stream_error_after_valid_json", true, delta + "data: {\"type\":\"error\",\"message\":\"synthetic provider failure\"}\n\n", false, 0},
		{"stream_failed_after_valid_json", true, delta + "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n", false, 0},
		{"stream_incomplete_after_valid_json", true, delta + "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n", false, 0},
		{"stream_conflicting_terminal_status", true, delta + "data: " + strings.Replace(completed, `"status":"completed"`, `"status":"failed"`, 1) + "\n\n", false, 0},
		{"stream_completed", true, delta + "data: " + completed + "\n\n", true, 17},
		{"stream_final_only", true, "data: " + completed + "\n\n", true, 17},
		{"stream_final_without_newline", true, "data: " + completed, true, 17},
		{"stream_multiline", true, "data: " + strings.Replace(completed, `,"response":`, ",\ndata: "+`"response":`, 1) + "\n\n", true, 17},
		{"stream_completed_without_usage", true, delta + "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output_text\":\"{}\"}}\n\n", true, 0},
		{"response_failed_with_valid_json", false, `{"status":"failed","output_text":"{}","usage":{"input_tokens":12,"output_tokens":5}}`, false, 0},
		{"response_incomplete_with_valid_json", false, `{"status":"incomplete","output_text":"{}","usage":{"input_tokens":12,"output_tokens":5}}`, false, 0},
		{"response_missing_status", false, `{"output_text":"{}","usage":{"input_tokens":12,"output_tokens":5}}`, false, 0},
		{"response_completed_without_usage", false, `{"status":"completed","output_text":"{}"}`, true, 0},
		{"response_partial_usage", false, `{"status":"completed","output_text":"{}","usage":{"input_tokens":12}}`, true, 0},
		{"response_negative_usage", false, `{"status":"completed","output_text":"{}","usage":{"input_tokens":12,"output_tokens":-5}}`, true, 0},
		{"response_overflowing_usage", false, `{"status":"completed","output_text":"{}","usage":{"input_tokens":9223372036854775807,"output_tokens":5}}`, true, 0},
		{"response_completed_with_error", false, `{"status":"completed","output_text":"{}","error":{"message":"Synthetic failure"}}`, false, 0},
		{"response_completed_with_refusal", false, `{"status":"completed","output_text":"{}","output":[{"content":[{"type":"refusal"}]}]}`, false, 0},
		{"response_completed", false, `{"status":"completed","output_text":"{}","usage":{"input_tokens":12,"output_tokens":5}}`, true, 17},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				contentType := "application/json"
				if test.stream {
					contentType = "text/event-stream"
				}
				writer.Header().Set("Content-Type", contentType)
				_, _ = fmt.Fprint(writer, test.body)
			}))
			defer upstream.Close()
			server := newTestServer()
			server.provider = ark.New(ark.Config{
				BaseURL: upstream.URL, APIKey: "fixture-key",
				Routes: map[contracts.Operation]ark.Route{contracts.OperationMealDecision: {Model: "fixture-model", TimeoutSeconds: 5}},
			}, upstream.Client(), nil)
			lease := &recordingQuotaLease{actualTokens: -1}
			server.quota = recordingQuota{lease: lease}
			path := "/v1/ai/requests"
			if test.stream {
				path = "/v1/ai/streams"
			}
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, authorizedRequest(http.MethodPost, path, validBody()))
			result := response.Body.String()
			if test.stream {
				if strings.Contains(result, "event: completed\n") != test.wantResult ||
					strings.Contains(result, "event: error\n") == test.wantResult {
					t.Errorf("incorrect terminal event for provider outcome: %s", result)
				}
			} else if (response.Code == http.StatusOK) != test.wantResult {
				t.Errorf("incorrect success status %d for provider outcome: %s", response.Code, result)
			}
			wantTokens := test.wantActual
			if wantTokens == 0 {
				var artifact contracts.Request
				if err := json.Unmarshal([]byte(validBody()), &artifact); err != nil {
					t.Fatal(err)
				}
				wantTokens = contracts.ReservationTokens(artifact)
			}
			if lease.actualTokens != wantTokens {
				t.Errorf("charged tokens = %d, want %d; incomplete or unmetered responses must retain their reservation", lease.actualTokens, wantTokens)
			}
		})
	}
}
