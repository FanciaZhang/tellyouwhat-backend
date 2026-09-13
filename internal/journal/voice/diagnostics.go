package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

// RewriteDiagnostics contains only provider and schema metadata. It must never
// contain a prompt, transcript, journal body, vocabulary item, credential, or
// provider response text.
type RewriteDiagnostics struct {
	Stage               string
	RequestedModel      string
	HTTPStatus          int
	ProviderRequestID   string
	ProviderErrorCode   string
	ProviderErrorType   string
	ResponseContentType string
	ResponseBytes       int
	ProviderStatus      string
	Duration            time.Duration
}

type rewriteDiagnosticError struct {
	clientCode  string
	diagnostics RewriteDiagnostics
	cause       error
}

func (e *rewriteDiagnosticError) Error() string { return e.clientCode }
func (e *rewriteDiagnosticError) Unwrap() error { return e.cause }

func failedRewrite(result RewriteResult, clientCode, stage string, cause error, started time.Time) (RewriteResult, error) {
	result.Diagnostics.Stage = stage
	result.Diagnostics.Duration = time.Since(started)
	return result, &rewriteDiagnosticError{clientCode: clientCode, diagnostics: result.Diagnostics, cause: cause}
}

func rewriteDiagnostics(result RewriteResult, err error) RewriteDiagnostics {
	if err == nil {
		return result.Diagnostics
	}
	var failure *rewriteDiagnosticError
	if errors.As(err, &failure) {
		return failure.diagnostics
	}
	return result.Diagnostics
}

func rewriteClientCode(err error) string {
	var failure *rewriteDiagnosticError
	if errors.As(err, &failure) && failure.clientCode != "" {
		return failure.clientCode
	}
	return "voice_rewrite_unavailable"
}

type rewriteTrace struct {
	ID      string
	Attempt int
}

type rewriteTraceContextKey struct{}

func withRewriteTrace(ctx context.Context, traceID string, attempt int) context.Context {
	return context.WithValue(ctx, rewriteTraceContextKey{}, rewriteTrace{ID: traceID, Attempt: attempt})
}

func rewriteTraceFrom(ctx context.Context) rewriteTrace {
	trace, _ := ctx.Value(rewriteTraceContextKey{}).(rewriteTrace)
	return trace
}

var diagnosticToken = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,160}$`)

func safeDiagnosticToken(value string) string {
	value = strings.TrimSpace(value)
	if diagnosticToken.MatchString(value) {
		return value
	}
	return ""
}

func providerRequestID(header http.Header) string {
	for _, name := range []string{"X-Tt-Logid", "X-Request-ID", "Request-ID", "Trace-ID", "X-Trace-ID"} {
		if value := strings.TrimSpace(header.Get(name)); diagnosticToken.MatchString(value) {
			return value
		}
	}
	return ""
}

func providerErrorMetadata(body []byte) (string, string) {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return "", ""
	}
	code, kind := envelope.Error.Code, envelope.Error.Type
	if code == "" {
		code = envelope.Code
	}
	if kind == "" {
		kind = envelope.Type
	}
	if !diagnosticToken.MatchString(code) {
		code = ""
	}
	if !diagnosticToken.MatchString(kind) {
		kind = ""
	}
	return code, kind
}

func errorClass(err error) string {
	if err == nil {
		return ""
	}
	for code, target := range map[string]error{
		"context_canceled": context.Canceled, "deadline_exceeded": context.DeadlineExceeded,
		"invalid_result": ErrInvalid, "revision_conflict": ErrConflict,
		"budget_exceeded": costcontrol.ErrBudgetExceeded, "concurrency_exceeded": costcontrol.ErrConcurrencyExceeded,
		"budget_configuration_conflict": costcontrol.ErrConfigurationConflict,
	} {
		if errors.Is(err, target) {
			return code
		}
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return "network_timeout"
		}
		return "network_error"
	}
	return fmt.Sprintf("%T", err)
}

func logRewriteProviderResult(logger *slog.Logger, ctx context.Context, callID string, result RewriteResult, err error) {
	if logger == nil {
		return
	}
	diagnostics := rewriteDiagnostics(result, err)
	trace := rewriteTraceFrom(ctx)
	attributes := []any{
		"provider_call_id", callID,
		"voice_trace_id", trace.ID,
		"rewrite_attempt", trace.Attempt,
		"outcome", map[bool]string{true: "failed", false: "completed"}[err != nil],
		"stage", diagnostics.Stage,
		"duration_ms", diagnostics.Duration.Milliseconds(),
		"http_status", diagnostics.HTTPStatus,
		"provider_request_id", diagnostics.ProviderRequestID,
		"provider_error_code", diagnostics.ProviderErrorCode,
		"provider_error_type", diagnostics.ProviderErrorType,
		"provider_status", diagnostics.ProviderStatus,
		"response_content_type", diagnostics.ResponseContentType,
		"response_bytes", diagnostics.ResponseBytes,
		"requested_model", safeDiagnosticToken(diagnostics.RequestedModel),
		"model", safeDiagnosticToken(result.Model),
		"config_version", safeDiagnosticToken(result.ConfigVersion),
		"input_token_count", result.InputTokens,
		"output_token_count", result.OutputTokens,
		"error_class", errorClass(err),
	}
	if err != nil {
		attributes = append(attributes, "client_error_code", rewriteClientCode(err))
		logger.WarnContext(ctx, "journal voice rewrite provider call", attributes...)
		return
	}
	logger.InfoContext(ctx, "journal voice rewrite provider call", attributes...)
}

func newVoiceTraceID() string { return uuid.NewString() }
