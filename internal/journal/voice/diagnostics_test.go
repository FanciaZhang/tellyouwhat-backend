package voice

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestArkRewriterLogsSafeProviderHTTPDiagnostics(t *testing.T) {
	const privateText = "不应出现在日志里的私人正文"
	const privateProviderMessage = "invalid input: 不应出现在日志里的私人正文"
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Tt-Logid", "ark-log-123")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"` + privateProviderMessage + `"}}`))
	}))
	defer server.Close()

	snapshot := Snapshot{Revision: 1, Blocks: []Block{{ID: uuid.NewString(), Text: privateText}}, Transcript: "私人转写"}
	result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "secret-api-key", Model: "model-endpoint", HTTP: server.Client(), Logger: logger}).Rewrite(context.Background(), snapshot, 7)
	if err == nil || err.Error() != "voice_rewrite_unavailable" {
		t.Fatalf("error = %v", err)
	}
	diagnostics := rewriteDiagnostics(result, err)
	if diagnostics.Stage != "provider_http_status" || diagnostics.HTTPStatus != 429 || diagnostics.ProviderRequestID != "ark-log-123" ||
		diagnostics.ProviderErrorCode != "rate_limit_exceeded" || diagnostics.ProviderErrorType != "rate_limit_error" || diagnostics.ResponseBytes == 0 {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	output := logs.String()
	for _, expected := range []string{"journal voice rewrite provider call", "provider_http_status", "ark-log-123", "rate_limit_exceeded", `"http_status":429`} {
		if !strings.Contains(output, expected) {
			t.Fatalf("log missing %q: %s", expected, output)
		}
	}
	for _, private := range []string{privateText, "私人转写", privateProviderMessage, "secret-api-key"} {
		if strings.Contains(output, private) {
			t.Fatalf("log leaked private value %q: %s", private, output)
		}
	}
}

func TestArkRewriterRetainsStructuredFailureStageAndUsage(t *testing.T) {
	var logs bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","model":"returned-model","usage":{"input_tokens":11,"output_tokens":22},"output":[{"content":[{"type":"output_text","text":"{}"}]}]}`))
	}))
	defer server.Close()

	result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "secret", Model: "requested-model", HTTP: server.Client(), Logger: slog.New(slog.NewJSONHandler(&logs, nil))}).Rewrite(
		context.Background(), Snapshot{Revision: 1, Blocks: []Block{{ID: uuid.NewString(), Text: "正文"}}}, 1,
	)
	if err == nil || result.InputTokens != 11 || result.OutputTokens != 22 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	diagnostics := rewriteDiagnostics(result, err)
	if diagnostics.Stage != "validate_revision_application" || diagnostics.ProviderStatus != "completed" || diagnostics.HTTPStatus != http.StatusOK {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	if errorClass(err) == "" {
		t.Fatalf("validation failure lost its error classification: %v", err)
	}
	output := logs.String()
	for _, expected := range []string{"validate_revision_application", `"input_token_count":11`, `"output_token_count":22`, "returned-model"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("log missing %q: %s", expected, output)
		}
	}
}
