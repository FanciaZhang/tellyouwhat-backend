package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/healthhttpapi"
	"github.com/tellyouwhat/backend/internal/jobs"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/platformhttpapi"
	"github.com/tellyouwhat/backend/internal/privacy"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/recognitionquota"
	"github.com/tellyouwhat/backend/internal/usage"
)

type apiFailure struct {
	status    int
	code      string
	message   string
	requestID string
}

func newAPIFailure(status int, code, message, requestID string) *apiFailure {
	return &apiFailure{status: status, code: code, message: message, requestID: strings.ToLower(requestID)}
}

func (failure *apiFailure) platformResponse() platformhttpapi.ErrorResponse {
	return platformhttpapi.ErrorResponse{Error: platformhttpapi.ErrorDetail{
		Code: failure.code, Message: failure.message, RequestID: failure.requestID,
	}}
}

func healthErrorResponse(failure *apiFailure) healthhttpapi.ErrorResponse {
	return healthhttpapi.ErrorResponse{Error: healthhttpapi.ErrorDetail{
		Code: failure.code, Message: failure.message, RequestID: failure.requestID,
	}}
}

func strictGinContext(ctx context.Context) *gin.Context {
	context, ok := ctx.(*gin.Context)
	if !ok {
		panic("strict gateway handler requires gin context")
	}
	return context
}

func (server *Server) apiAuthenticate(ctx context.Context, requestID uuid.UUID) (Principal, *apiFailure) {
	requestIDString := requestID.String()
	if server.authenticator == nil {
		return Principal{}, newAPIFailure(http.StatusServiceUnavailable, "not_ready", "authentication service unavailable", requestIDString)
	}
	ginContext := strictGinContext(ctx)
	body := rawRequestBody(ginContext)
	proof := RequestProof{
		Method:     ginContext.Request.Method,
		Path:       ginContext.Request.URL.EscapedPath(),
		RequestID:  requestIDString,
		KeyID:      ginContext.GetHeader("X-Tellyouwhat-Key-ID"),
		Assertion:  ginContext.GetHeader("X-Tellyouwhat-Assertion"),
		Nonce:      ginContext.GetHeader("X-Tellyouwhat-Nonce"),
		Timestamp:  ginContext.GetHeader("X-Tellyouwhat-Timestamp"),
		BodySHA256: contracts.BodySHA256(body),
	}
	principal, err := server.authenticator.Authenticate(ctx, proof)
	if err == nil {
		if principal.AppID != string(server.app.ID) {
			return Principal{}, newAPIFailure(http.StatusUnauthorized, "authentication_failed", "request authentication failed", requestIDString)
		}
		return principal, nil
	}
	switch {
	case errors.Is(err, attestation.ErrReplay):
		return Principal{}, newAPIFailure(http.StatusConflict, "replay_detected", "request nonce or assertion was already used", requestIDString)
	case errors.Is(err, attestation.ErrUnavailable):
		return Principal{}, newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", requestIDString)
	default:
		return Principal{}, newAPIFailure(http.StatusUnauthorized, "authentication_failed", "request authentication failed", requestIDString)
	}
}

func (server *Server) apiRequireManagedEntitlement(ctx context.Context, principal Principal, requestID string) *apiFailure {
	allowed, failure := server.apiManagedEntitlement(ctx, principal, requestID)
	if failure != nil {
		return failure
	}
	if !allowed {
		return newAPIFailure(http.StatusForbidden, "managed_subscription_required", "managed subscription required", requestID)
	}
	return nil
}

func (server *Server) apiManagedEntitlement(ctx context.Context, principal Principal, requestID string) (bool, *apiFailure) {
	if server.entitlements == nil {
		return false, newAPIFailure(http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", requestID)
	}
	allowed, err := server.entitlements.HasManagedSubscription(ctx, principal)
	if err != nil {
		return false, newAPIFailure(http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", requestID)
	}
	return allowed, nil
}

func (server *Server) apiValidateAIRequest(
	ctx context.Context,
	requestID uuid.UUID,
	body *healthhttpapi.AIRequest,
) (contracts.Request, []byte, Principal, bool, *apiFailure) {
	requestIDString := requestID.String()
	if server.authenticator == nil || server.entitlements == nil || server.quota == nil || server.provider == nil || server.contracts == nil || server.media == nil || server.usage == nil {
		return contracts.Request{}, nil, Principal{}, false, newAPIFailure(http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", requestIDString)
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return contracts.Request{}, nil, Principal{}, false, failure
	}
	artifact, failure := apiRequest(body, requestIDString)
	if failure != nil {
		return contracts.Request{}, nil, Principal{}, false, failure
	}
	if err := server.contracts.Validate(artifact); err != nil {
		return contracts.Request{}, nil, Principal{}, false, mappedContractFailure(err, requestIDString)
	}
	if !server.app.AllowsOperation(string(artifact.Operation)) {
		return contracts.Request{}, nil, Principal{}, false, newAPIFailure(
			http.StatusUnprocessableEntity,
			"contract_violation",
			"operation is not available for this application",
			requestIDString,
		)
	}
	managed, failure := server.apiAuthorizeAIEntitlement(ctx, principal, artifact, requestIDString)
	if failure != nil {
		return contracts.Request{}, nil, Principal{}, false, failure
	}
	return artifact, rawRequestBody(strictGinContext(ctx)), principal, managed, nil
}

func apiRequest(body *healthhttpapi.AIRequest, requestID string) (contracts.Request, *apiFailure) {
	if body == nil {
		return contracts.Request{}, newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", requestID)
	}
	if body.RequestID.String() != requestID {
		return contracts.Request{}, newAPIFailure(http.StatusUnauthorized, "authentication_failed", "request authentication failed", requestID)
	}
	responseSchema, err := json.Marshal(body.ResponseSchema)
	if err != nil {
		return contracts.Request{}, newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "response schema is invalid", requestID)
	}
	artifact := contracts.Request{
		RequestID:         body.RequestID.String(),
		Operation:         contracts.Operation(body.Operation),
		ContractVersion:   string(body.ContractVersion),
		PromptVersion:     body.PromptVersion,
		Prompt:            body.Prompt,
		ResponseSchema:    responseSchema,
		SemanticSignature: body.SemanticSignature,
		Options: contracts.RequestOptions{
			Temperature: body.Options.Temperature,
		},
		Media: make([]contracts.Media, 0, len(body.Media)),
	}
	if body.RecognitionSession != nil {
		artifact.RecognitionSession = &contracts.RecognitionSessionContext{
			SessionID:            body.RecognitionSession.SessionID.String(),
			BusinessDayStartHour: body.RecognitionSession.BusinessDayStartHour,
			TimeZoneIdentifier:   body.RecognitionSession.TimeZoneIdentifier,
		}
	}
	if body.Options.Stream != nil {
		artifact.Options.Stream = *body.Options.Stream
	}
	if body.Options.ReasoningEffort != nil {
		artifact.Options.ReasoningEffort = string(*body.Options.ReasoningEffort)
	}
	if body.Options.WebSearchEnabled != nil {
		artifact.Options.WebSearchEnabled = *body.Options.WebSearchEnabled
	}
	for _, item := range body.Media {
		artifact.Media = append(artifact.Media, contracts.Media{
			ID: item.Id, Kind: string(item.Kind), MIMEType: item.MimeType,
			ObjectID: item.ObjectID, SHA256: item.Sha256, SizeBytes: item.SizeBytes,
		})
	}
	if err := artifact.Validate(); err != nil {
		return contracts.Request{}, mappedContractFailure(err, requestID)
	}
	return artifact, nil
}

func (server *Server) apiAuthorizeAIEntitlement(
	ctx context.Context,
	principal Principal,
	artifact contracts.Request,
	requestID string,
) (bool, *apiFailure) {
	managed, failure := server.apiManagedEntitlement(ctx, principal, requestID)
	if failure != nil {
		return false, failure
	}
	if managed {
		if failure := server.apiRequireConsents(ctx, principal, server.consentScopes(privacy.ManagedAIScope), requestID); failure != nil {
			return false, failure
		}
		return true, nil
	}
	if !contracts.IsMealRecognitionOperation(artifact.Operation) || artifact.RecognitionSession == nil {
		return false, newAPIFailure(http.StatusForbidden, "premium_required", "premium entitlement required", requestID)
	}
	if failure := server.apiRequireConsents(ctx, principal, server.consentScopes(privacy.FreeRecognitionScope), requestID); failure != nil {
		return false, failure
	}
	if server.recognitionSessions == nil || server.freeRecognitionQuota == nil {
		return false, newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID)
	}
	_, err := server.recognitionSessions.Reserve(ctx, recognitionquota.Request{
		DeviceID: principal.DeviceID,
		Context: recognitionquota.Context{
			SessionID:            artifact.RecognitionSession.SessionID,
			BusinessDayStartHour: artifact.RecognitionSession.BusinessDayStartHour,
			TimeZoneIdentifier:   artifact.RecognitionSession.TimeZoneIdentifier,
		},
	}, server.now())
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, recognitionquota.ErrExceeded):
		return false, newAPIFailure(http.StatusTooManyRequests, "free_recognition_quota_exceeded", "three free meal recognitions have already been used or reserved for this business day", requestID)
	case errors.Is(err, recognitionquota.ErrInvalid):
		return false, newAPIFailure(http.StatusUnprocessableEntity, "invalid_recognition_session", "recognition session is invalid", requestID)
	default:
		return false, newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID)
	}
}

func (server *Server) consentScopes(extra string) []string {
	scopes := make([]string, 0, len(server.requiredConsentScopes)+1)
	scopes = append(scopes, server.requiredConsentScopes...)
	return append(scopes, extra)
}

func (server *Server) apiRequireConsents(
	ctx context.Context,
	principal Principal,
	scopes []string,
	requestID string,
) *apiFailure {
	if len(scopes) == 0 {
		return nil
	}
	if server.consent == nil {
		return newAPIFailure(http.StatusServiceUnavailable, "consent_unavailable", "privacy consent service unavailable", requestID)
	}
	granted, err := server.consent.HasRequiredConsents(ctx, principal, scopes)
	if err != nil {
		return newAPIFailure(http.StatusServiceUnavailable, "consent_unavailable", "privacy consent service unavailable", requestID)
	}
	if !granted {
		return newAPIFailure(http.StatusForbidden, "consent_required", "required privacy consent has not been granted", requestID)
	}
	return nil
}

func mappedContractFailure(err error, requestID string) *apiFailure {
	switch {
	case errors.Is(err, contracts.ErrPayloadTooLarge):
		return newAPIFailure(http.StatusRequestEntityTooLarge, "payload_too_large", "request exceeds the operation limit", requestID)
	case errors.Is(err, contracts.ErrUpgradeRequired):
		return newAPIFailure(http.StatusUpgradeRequired, "upgrade_required", "this app version is no longer supported", requestID)
	default:
		return newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request violates the business contract", requestID)
	}
}

func (server *Server) apiAcquireQuota(
	ctx context.Context,
	principal Principal,
	artifact contracts.Request,
	reservationID string,
	managed bool,
) (quota.Releaser, *apiFailure) {
	quotaService := server.quota
	if !managed {
		quotaService = server.freeRecognitionQuota
	}
	if quotaService == nil {
		return nil, newAPIFailure(http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", artifact.RequestID)
	}
	quotaPrincipal := principalForQuota(principal, managed)
	ginContext := strictGinContext(ctx)
	lease, err := quotaService.Acquire(ctx, quota.Identity{
		DeviceID:      quotaPrincipal.DeviceID,
		TransactionID: quotaPrincipal.TransactionID,
		IP:            server.ipResolver(ginContext.Request),
	}, artifact.Operation, contracts.ReservationTokens(artifact), reservationID, server.now())
	if err == nil {
		return lease, nil
	}
	if !errors.Is(err, quota.ErrExceeded) {
		return nil, newAPIFailure(http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", artifact.RequestID)
	}
	code, message := quotaExceededResponse(err)
	return nil, newAPIFailure(http.StatusTooManyRequests, code, message, artifact.RequestID)
}

func quotaExceededResponse(err error) (string, string) {
	scope, _ := quota.ExceededScope(err)
	switch scope {
	case quota.LimitDailyTokens:
		return "daily_quota_exceeded", "daily managed AI safety limit reached"
	case quota.LimitMonthlyTokens:
		return "monthly_quota_exceeded", "monthly managed AI safety limit reached"
	case quota.LimitConcurrentDevice:
		return "concurrency_limit_exceeded", "managed AI requests are already in progress"
	case quota.LimitRequestsPerMinuteIP,
		quota.LimitRequestsPerMinuteDevice,
		quota.LimitRequestsPerMinuteOperation:
		return "rate_limit_exceeded", "managed AI requests are arriving too quickly"
	default:
		return "quota_exceeded", "managed AI quota exceeded"
	}
}

func capabilityQuotaReservationID(principal Principal, requestID, bodyDigest string) string {
	return quota.JobReservationID(principal.KeyID, requestID, bodyDigest)
}

func (server *Server) apiAdmissionFailure(err error, requestID string) *apiFailure {
	switch {
	case errors.Is(err, media.ErrIdempotencyReplay), errors.Is(err, media.ErrIdempotencyConflict):
		return newAPIFailure(http.StatusConflict, "idempotency_conflict", "requestID was already used", requestID)
	case errors.Is(err, media.ErrNotAuthorized):
		return newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request violates the managed media contract", requestID)
	default:
		return newAPIFailure(http.StatusServiceUnavailable, "request_commit_unavailable", "request authorization could not be committed", requestID)
	}
}

func (server *Server) recordUsage(
	ctx context.Context,
	principal Principal,
	request contracts.Request,
	managed bool,
	inputTokens,
	outputTokens int,
) error {
	quotaPrincipal := principalForQuota(principal, managed)
	return server.usage.Record(ctx, usage.Record{
		RequestID:     request.RequestID,
		KeyID:         principal.KeyID,
		DeviceID:      principal.DeviceID,
		TransactionID: quotaPrincipal.TransactionID,
		Operation:     request.Operation,
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
		OccurredAt:    server.now(),
	})
}

func principalForQuota(principal Principal, managed bool) Principal {
	if managed {
		if principal.TransactionID == "" {
			principal.TransactionID = principal.KeyID
		}
		return principal
	}
	principal.TransactionID = quota.FreeRecognitionTransactionPrefix + principal.KeyID
	return principal
}

func cleanupManagedMedia(ctx context.Context, provider Provider, values []contracts.Media) {
	cleaner, ok := provider.(providerapi.ManagedMediaCleaner)
	if !ok || len(values) == 0 {
		return
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	cleaner.CleanupManagedMedia(cleanupContext, values)
}

func apiJob(job jobs.Job) (healthhttpapi.AIJob, error) {
	jobID, err := uuid.Parse(job.ID)
	if err != nil {
		return healthhttpapi.AIJob{}, fmt.Errorf("parse job ID: %w", err)
	}
	requestID, err := uuid.Parse(job.RequestID)
	if err != nil {
		return healthhttpapi.AIJob{}, fmt.Errorf("parse job request ID: %w", err)
	}
	value := healthhttpapi.AIJob{
		JobID: jobID, RequestID: requestID, Status: healthhttpapi.AIJobStatus(job.Status),
		CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
	}
	if job.Status == jobs.StatusSucceeded {
		value.Content = &job.Result
		value.Usage = &healthhttpapi.TokenUsage{InputTokens: job.InputTokens, OutputTokens: job.OutputTokens}
	}
	if job.Status == jobs.StatusFailed {
		value.ErrorCategory = &job.FailureCategory
	}
	return value, nil
}

func writeSSE(writer io.Writer, event string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if strings.ContainsAny(event, "\r\n") {
		return errors.New("invalid SSE event")
	}
	_, err = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, encoded)
	return err
}
