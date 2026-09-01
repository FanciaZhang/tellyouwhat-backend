package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/httpapi"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/recognitionquota"
)

func (server *Server) GetHealth(
	context.Context,
	httpapi.GetHealthRequestObject,
) (httpapi.GetHealthResponseObject, error) {
	return httpapi.GetHealth200JSONResponse{Status: httpapi.ServiceStatusStatusOk}, nil
}

func (server *Server) GetReadiness(
	ctx context.Context,
	_ httpapi.GetReadinessRequestObject,
) (httpapi.GetReadinessResponseObject, error) {
	if server.authenticator == nil || server.entitlements == nil || server.quota == nil || server.quotaReader == nil ||
		server.freeRecognitionQuota == nil || server.freeRecognitionQuotaReader == nil || server.recognitionSessions == nil || server.provider == nil ||
		server.enrollment == nil || server.media == nil || server.jobs == nil || server.dispatcher == nil || server.capabilities == nil || server.contracts == nil || server.usage == nil || server.readiness == nil || server.privacy == nil || server.consent == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", "")
		return httpapi.GetReadiness503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	readinessContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := server.readiness.Ready(readinessContext); err != nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", "")
		return httpapi.GetReadiness503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	return httpapi.GetReadiness200JSONResponse{Status: httpapi.ServiceStatusStatusReady}, nil
}

func (server *Server) GetManagedAIProduct(
	context.Context,
	httpapi.GetManagedAIProductRequestObject,
) (httpapi.GetManagedAIProductResponseObject, error) {
	product := server.managedProduct
	return httpapi.GetManagedAIProduct200JSONResponse{
		ProductID: product.ProductID, BillingPeriod: product.BillingPeriod,
		DailyTokenLimit: product.DailyTokenLimit, MonthlyTokenLimit: product.MonthlyTokenLimit,
		Provider: product.Provider, ModelDisclosure: product.ModelDisclosure,
		MediaRetention: product.MediaRetention, JobRetention: product.JobRetention,
		PrivacyURL: product.PrivacyURL, TermsURL: product.TermsURL,
		PrivacyChoicesURL: product.PrivacyChoicesURL, SupportURL: product.SupportURL,
	}, nil
}

func (server *Server) IssueAttestationChallenge(
	ctx context.Context,
	request httpapi.IssueAttestationChallengeRequestObject,
) (httpapi.IssueAttestationChallengeResponseObject, error) {
	if server.enrollment == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return httpapi.IssueAttestationChallenge503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	if request.Body == nil {
		failure := newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", "")
		return httpapi.IssueAttestationChallenge422JSONResponse{UnprocessableEntityJSONResponse: httpapi.UnprocessableEntityJSONResponse(failure.response())}, nil
	}
	challenge, err := server.enrollment.IssueChallenge(ctx, request.Body.KeyID)
	if err != nil {
		if errors.Is(err, attestation.ErrUnavailable) {
			failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
			return httpapi.IssueAttestationChallenge503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
		}
		failure := newAPIFailure(http.StatusUnauthorized, "enrollment_denied", "device enrollment denied", "")
		return httpapi.IssueAttestationChallenge401JSONResponse{UnauthorizedJSONResponse: httpapi.UnauthorizedJSONResponse(failure.response())}, nil
	}
	return httpapi.IssueAttestationChallenge201JSONResponse{
		Challenge: challenge.Value, ExpiresAt: challenge.ExpiresAt,
	}, nil
}

func (server *Server) RegisterAttestationKey(
	ctx context.Context,
	request httpapi.RegisterAttestationKeyRequestObject,
) (httpapi.RegisterAttestationKeyResponseObject, error) {
	if server.enrollment == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return httpapi.RegisterAttestationKey503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	if request.Body == nil {
		failure := newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", "")
		return httpapi.RegisterAttestationKey422JSONResponse{UnprocessableEntityJSONResponse: httpapi.UnprocessableEntityJSONResponse(failure.response())}, nil
	}
	principal, err := server.enrollment.Register(ctx, attestation.RegistrationRequest{
		KeyID: request.Body.KeyID, Challenge: request.Body.Challenge,
		Attestation: base64.StdEncoding.EncodeToString(request.Body.Attestation),
		Build:       request.Body.Build, ActivationSecret: request.Body.ActivationSecret,
	})
	if err != nil {
		switch {
		case errors.Is(err, attestation.ErrUnavailable):
			failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
			return httpapi.RegisterAttestationKey503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
		case errors.Is(err, attestation.ErrReplay):
			failure := newAPIFailure(http.StatusConflict, "replay_detected", "registration challenge was already used", "")
			return httpapi.RegisterAttestationKey409JSONResponse{ConflictJSONResponse: httpapi.ConflictJSONResponse(failure.response())}, nil
		case errors.Is(err, attestation.ErrKeyAlreadyRegistered):
			failure := newAPIFailure(http.StatusConflict, "key_already_registered", "App Attest key is already registered", "")
			return httpapi.RegisterAttestationKey409JSONResponse{ConflictJSONResponse: httpapi.ConflictJSONResponse(failure.response())}, nil
		default:
			failure := newAPIFailure(http.StatusUnauthorized, "enrollment_denied", "device enrollment denied", "")
			return httpapi.RegisterAttestationKey401JSONResponse{UnauthorizedJSONResponse: httpapi.UnauthorizedJSONResponse(failure.response())}, nil
		}
	}
	deviceID, err := uuid.Parse(principal.DeviceID)
	if err != nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return httpapi.RegisterAttestationKey503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	return httpapi.RegisterAttestationKey201JSONResponse{KeyID: principal.KeyID, DeviceID: deviceID}, nil
}

func (server *Server) ActivateDevelopmentEntitlement(
	ctx context.Context,
	request httpapi.ActivateDevelopmentEntitlementRequestObject,
) (httpapi.ActivateDevelopmentEntitlementResponseObject, error) {
	requestID := request.Params.XHealthRequestID
	if server.activator == nil {
		failure := newAPIFailure(http.StatusNotFound, "not_found", "route not found", requestID.String())
		return httpapi.ActivateDevelopmentEntitlement404JSONResponse{NotFoundJSONResponse: httpapi.NotFoundJSONResponse(failure.response())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return httpapi.ActivateDevelopmentEntitlementdefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	expiresAt, err := server.activator.Activate(ctx, principal, request.Params.XHealthDevActivation)
	if err != nil {
		if errors.Is(err, entitlement.ErrActivationDenied) {
			failure = newAPIFailure(http.StatusForbidden, "activation_denied", "development entitlement activation denied", requestID.String())
			return httpapi.ActivateDevelopmentEntitlement403JSONResponse{ForbiddenJSONResponse: httpapi.ForbiddenJSONResponse(failure.response())}, nil
		}
		failure = newAPIFailure(http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", requestID.String())
		return httpapi.ActivateDevelopmentEntitlement503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	return httpapi.ActivateDevelopmentEntitlement200JSONResponse{Status: httpapi.Active, ExpiresAt: expiresAt}, nil
}

func (server *Server) SynchronizeSubscriptionTransaction(
	ctx context.Context,
	request httpapi.SynchronizeSubscriptionTransactionRequestObject,
) (httpapi.SynchronizeSubscriptionTransactionResponseObject, error) {
	requestID := request.Params.XHealthRequestID
	if server.productionEntitlement == nil {
		failure := newAPIFailure(http.StatusNotFound, "not_found", "route not found", requestID.String())
		return httpapi.SynchronizeSubscriptionTransaction404JSONResponse{NotFoundJSONResponse: httpapi.NotFoundJSONResponse(failure.response())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return httpapi.SynchronizeSubscriptionTransactiondefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	if request.Body == nil {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", requestID.String())
		return httpapi.SynchronizeSubscriptionTransaction422JSONResponse{UnprocessableEntityJSONResponse: httpapi.UnprocessableEntityJSONResponse(failure.response())}, nil
	}
	expiresAt, err := server.productionEntitlement.Sync(ctx, principal, request.Body.SignedTransaction)
	if err != nil {
		switch {
		case errors.Is(err, entitlement.ErrProductionSyncDenied):
			failure = newAPIFailure(http.StatusForbidden, "transaction_verification_failed", "subscription transaction verification failed", requestID.String())
		case errors.Is(err, entitlement.ErrSubscriptionInactive):
			failure = newAPIFailure(http.StatusForbidden, "managed_subscription_required", "managed subscription required", requestID.String())
		default:
			failure = newAPIFailure(http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", requestID.String())
		}
		return httpapi.SynchronizeSubscriptionTransactiondefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	return httpapi.SynchronizeSubscriptionTransaction200JSONResponse{Status: httpapi.Active, ExpiresAt: expiresAt}, nil
}

func (server *Server) ProcessAppStoreNotification(
	ctx context.Context,
	request httpapi.ProcessAppStoreNotificationRequestObject,
) (httpapi.ProcessAppStoreNotificationResponseObject, error) {
	if server.appStoreNotifications == nil {
		failure := newAPIFailure(http.StatusNotFound, "not_found", "route not found", "")
		return httpapi.ProcessAppStoreNotification404JSONResponse{NotFoundJSONResponse: httpapi.NotFoundJSONResponse(failure.response())}, nil
	}
	if request.Body == nil {
		failure := newAPIFailure(http.StatusBadRequest, "invalid_notification_envelope", "notification envelope is invalid", "")
		return httpapi.ProcessAppStoreNotification400JSONResponse{BadRequestJSONResponse: httpapi.BadRequestJSONResponse(failure.response())}, nil
	}
	if _, err := server.appStoreNotifications.Process(ctx, request.Body.SignedPayload); err != nil {
		if errors.Is(err, entitlement.ErrProductionSyncDenied) {
			failure := newAPIFailure(http.StatusBadRequest, "notification_verification_failed", "notification verification failed", "")
			return httpapi.ProcessAppStoreNotification400JSONResponse{BadRequestJSONResponse: httpapi.BadRequestJSONResponse(failure.response())}, nil
		}
		failure := newAPIFailure(http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", "")
		return httpapi.ProcessAppStoreNotification503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	return httpapi.ProcessAppStoreNotification200JSONResponse{Status: httpapi.OKStatusStatusOk}, nil
}

func (server *Server) RecordPrivacyConsents(
	ctx context.Context,
	request httpapi.RecordPrivacyConsentsRequestObject,
) (httpapi.RecordPrivacyConsentsResponseObject, error) {
	requestID := request.Params.XHealthRequestID
	if server.privacy == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", requestID.String())
		return httpapi.RecordPrivacyConsents503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return httpapi.RecordPrivacyConsentsdefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	if request.Body == nil {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", requestID.String())
		return httpapi.RecordPrivacyConsents422JSONResponse{UnprocessableEntityJSONResponse: httpapi.UnprocessableEntityJSONResponse(failure.response())}, nil
	}
	consents := make([]privacy.Consent, 0, len(request.Body.Consents))
	for _, consent := range request.Body.Consents {
		consents = append(consents, privacy.Consent{
			Scope: string(consent.Scope), DocumentVersion: consent.DocumentVersion, Granted: consent.Granted,
		})
	}
	recordedAt, err := server.privacy.RecordConsents(ctx, principal, consents)
	if errors.Is(err, privacy.ErrInvalidConsent) {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "invalid_consent", "consent scope or document version is invalid", requestID.String())
		return httpapi.RecordPrivacyConsents422JSONResponse{UnprocessableEntityJSONResponse: httpapi.UnprocessableEntityJSONResponse(failure.response())}, nil
	}
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", requestID.String())
		return httpapi.RecordPrivacyConsents503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	return httpapi.RecordPrivacyConsents200JSONResponse{RecordedAt: recordedAt, Count: len(consents)}, nil
}

func (server *Server) DeletePrivacyData(
	ctx context.Context,
	request httpapi.DeletePrivacyDataRequestObject,
) (httpapi.DeletePrivacyDataResponseObject, error) {
	requestID := request.Params.XHealthRequestID
	if server.privacy == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", requestID.String())
		return httpapi.DeletePrivacyData503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return httpapi.DeletePrivacyDatadefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	if err := server.privacy.DeletePrincipal(ctx, principal); err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "deletion_unavailable", "data deletion could not be completed", requestID.String())
		return httpapi.DeletePrivacyData503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	return httpapi.DeletePrivacyData204Response{}, nil
}

func (server *Server) AuthorizeMediaUpload(
	ctx context.Context,
	request httpapi.AuthorizeMediaUploadRequestObject,
) (httpapi.AuthorizeMediaUploadResponseObject, error) {
	requestID := request.Params.XHealthRequestID
	if server.media == nil || server.entitlements == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "media_unavailable", "media service unavailable", requestID.String())
		return httpapi.AuthorizeMediaUpload503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return httpapi.AuthorizeMediaUploaddefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	if request.Body == nil || request.Body.RequestID != requestID {
		failure = newAPIFailure(http.StatusUnauthorized, "authentication_failed", "request authentication failed", requestID.String())
		return httpapi.AuthorizeMediaUpload401JSONResponse{UnauthorizedJSONResponse: httpapi.UnauthorizedJSONResponse(failure.response())}, nil
	}
	artifact := contracts.Request{Operation: contracts.Operation(request.Body.Operation)}
	if !server.app.AllowsOperation(string(artifact.Operation)) {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "operation is not available for this application", requestID.String())
		return httpapi.AuthorizeMediaUpload422JSONResponse{UnprocessableEntityJSONResponse: httpapi.UnprocessableEntityJSONResponse(failure.response())}, nil
	}
	if request.Body.RecognitionSession != nil {
		artifact.RecognitionSession = &contracts.RecognitionSessionContext{
			SessionID:            request.Body.RecognitionSession.SessionID.String(),
			BusinessDayStartHour: request.Body.RecognitionSession.BusinessDayStartHour,
			TimeZoneIdentifier:   request.Body.RecognitionSession.TimeZoneIdentifier,
		}
	}
	if _, failure = server.apiAuthorizeAIEntitlement(ctx, principal, artifact, requestID.String()); failure != nil {
		return httpapi.AuthorizeMediaUploaddefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	authorization, err := server.media.Authorize(ctx, principal, media.UploadRequest{
		RequestID: request.Body.RequestID.String(), Operation: contracts.Operation(request.Body.Operation),
		MediaID: request.Body.MediaID, Kind: string(request.Body.Kind), MIMEType: string(request.Body.MimeType),
		SHA256: request.Body.Sha256, SizeBytes: request.Body.SizeBytes,
	})
	if err != nil {
		switch {
		case errors.Is(err, contracts.ErrContractViolation), errors.Is(err, contracts.ErrPayloadTooLarge):
			failure = mappedContractFailure(err, requestID.String())
		case errors.Is(err, media.ErrAuthorizationConflict):
			failure = newAPIFailure(http.StatusConflict, "media_authorization_conflict", "media authorization already exists with different metadata", requestID.String())
		default:
			failure = newAPIFailure(http.StatusServiceUnavailable, "media_unavailable", "media service unavailable", requestID.String())
		}
		return httpapi.AuthorizeMediaUploaddefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	return httpapi.AuthorizeMediaUpload201JSONResponse{
		ObjectID: authorization.ObjectID, UploadURL: authorization.UploadURL,
		RequiredHeaders: authorization.RequiredHeaders, ExpiresAt: authorization.ExpiresAt,
	}, nil
}

func (server *Server) GetAIQuota(
	ctx context.Context,
	request httpapi.GetAIQuotaRequestObject,
) (httpapi.GetAIQuotaResponseObject, error) {
	requestID := request.Params.XHealthRequestID
	if server.quotaReader == nil || server.freeRecognitionQuotaReader == nil || server.recognitionSessions == nil || server.entitlements == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", requestID.String())
		return httpapi.GetAIQuota503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return httpapi.GetAIQuotadefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	managed, failure := server.apiManagedEntitlement(ctx, principal, requestID.String())
	if failure != nil {
		return httpapi.GetAIQuotadefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	reader := server.quotaReader
	plan := httpapi.QuotaSnapshotPlanManagedSubscription
	if !managed {
		reader = server.freeRecognitionQuotaReader
		plan = httpapi.QuotaSnapshotPlanFree
	}
	quotaPrincipal := principalForQuota(principal, managed)
	snapshot, err := reader.Snapshot(ctx, quotaPrincipal.TransactionID, server.now())
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", requestID.String())
		return httpapi.GetAIQuota503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	startHour := 4
	if request.Params.BusinessDayStartHour != nil {
		startHour = *request.Params.BusinessDayStartHour
	}
	timeZoneIdentifier := "Asia/Shanghai"
	if request.Params.TimeZoneIdentifier != nil {
		timeZoneIdentifier = *request.Params.TimeZoneIdentifier
	}
	recognition, err := server.recognitionSessions.Snapshot(ctx, principal.DeviceID, recognitionquota.WindowSettings{
		BusinessDayStartHour: startHour, TimeZoneIdentifier: timeZoneIdentifier,
	}, server.now())
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID.String())
		return httpapi.GetAIQuota503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	return httpapi.GetAIQuota200JSONResponse{
		Plan:      plan,
		DailyUsed: snapshot.DailyUsed, DailyLimit: snapshot.DailyLimit, DailyResetAt: snapshot.DailyResetAt,
		MonthlyUsed: snapshot.MonthlyUsed, MonthlyLimit: snapshot.MonthlyLimit, MonthlyResetAt: snapshot.MonthlyResetAt,
		RecognitionCompleted: recognition.Completed, RecognitionReserved: recognition.Reserved,
		RecognitionRemaining: recognition.Remaining, RecognitionResetAt: recognition.ResetAt,
	}, nil
}

func (server *Server) CompleteRecognitionSession(
	ctx context.Context,
	request httpapi.CompleteRecognitionSessionRequestObject,
) (httpapi.CompleteRecognitionSessionResponseObject, error) {
	requestID := request.Params.XHealthRequestID
	if server.recognitionSessions == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID.String())
		return httpapi.CompleteRecognitionSession503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return httpapi.CompleteRecognitionSessiondefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	snapshot, err := server.recognitionSessions.Complete(ctx, principal.DeviceID, request.Id.String(), server.now())
	if errors.Is(err, recognitionquota.ErrNotFound) {
		failure = newAPIFailure(http.StatusNotFound, "recognition_session_not_found", "recognition session was not found or has expired", requestID.String())
		return httpapi.CompleteRecognitionSession404JSONResponse{NotFoundJSONResponse: httpapi.NotFoundJSONResponse(failure.response())}, nil
	}
	if errors.Is(err, recognitionquota.ErrInvalid) {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "invalid_recognition_session", "recognition session is invalid", requestID.String())
		return httpapi.CompleteRecognitionSession422JSONResponse{UnprocessableEntityJSONResponse: httpapi.UnprocessableEntityJSONResponse(failure.response())}, nil
	}
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID.String())
		return httpapi.CompleteRecognitionSession503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	return httpapi.CompleteRecognitionSession200JSONResponse{
		Completed: snapshot.Completed, Reserved: snapshot.Reserved,
		Remaining: snapshot.Remaining, ResetAt: snapshot.ResetAt,
	}, nil
}

func (server *Server) CancelRecognitionSession(
	ctx context.Context,
	request httpapi.CancelRecognitionSessionRequestObject,
) (httpapi.CancelRecognitionSessionResponseObject, error) {
	requestID := request.Params.XHealthRequestID
	if server.recognitionSessions == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID.String())
		return httpapi.CancelRecognitionSession503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return httpapi.CancelRecognitionSessiondefaultJSONResponse{Body: failure.response(), StatusCode: failure.status}, nil
	}
	if err := server.recognitionSessions.Cancel(ctx, principal.DeviceID, request.Id.String(), server.now()); err != nil {
		if errors.Is(err, recognitionquota.ErrInvalid) {
			failure = newAPIFailure(http.StatusUnprocessableEntity, "invalid_recognition_session", "recognition session is invalid", requestID.String())
			return httpapi.CancelRecognitionSession422JSONResponse{UnprocessableEntityJSONResponse: httpapi.UnprocessableEntityJSONResponse(failure.response())}, nil
		}
		failure = newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID.String())
		return httpapi.CancelRecognitionSession503JSONResponse{ServiceUnavailableJSONResponse: httpapi.ServiceUnavailableJSONResponse(failure.response())}, nil
	}
	return httpapi.CancelRecognitionSession204Response{}, nil
}
