package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/platformhttpapi"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/recognitionquota"
)

var _ platformhttpapi.StrictServerInterface = (*Server)(nil)

func (server *Server) GetHealth(
	context.Context,
	platformhttpapi.GetHealthRequestObject,
) (platformhttpapi.GetHealthResponseObject, error) {
	return platformhttpapi.GetHealth200JSONResponse{Status: platformhttpapi.ServiceStatusStatusOk}, nil
}

func (server *Server) GetReadiness(
	ctx context.Context,
	_ platformhttpapi.GetReadinessRequestObject,
) (platformhttpapi.GetReadinessResponseObject, error) {
	if !server.dependenciesAvailable() {
		failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", "")
		return platformhttpapi.GetReadiness503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	readinessContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := server.readiness.Ready(readinessContext); err != nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", "")
		return platformhttpapi.GetReadiness503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	return platformhttpapi.GetReadiness200JSONResponse{Status: platformhttpapi.ServiceStatusStatusReady}, nil
}

func (server *Server) dependenciesAvailable() bool {
	common := server.authenticator != nil && server.entitlements != nil && server.quota != nil && server.quotaReader != nil &&
		server.enrollment != nil && server.usage != nil && server.readiness != nil && server.privacy != nil && server.consent != nil && server.media != nil
	if !common {
		return false
	}
	switch server.app.ID {
	case appregistry.Health:
		return server.freeRecognitionQuota != nil && server.freeRecognitionQuotaReader != nil && server.recognitionSessions != nil &&
			server.provider != nil && server.jobs != nil && server.dispatcher != nil && server.capabilities != nil && server.contracts != nil
	case appregistry.Journal:
		return server.journalOrganizer != nil && server.journalAnalysisVersion != ""
	default:
		return false
	}
}

func (server *Server) GetManagedAIProduct(
	context.Context,
	platformhttpapi.GetManagedAIProductRequestObject,
) (platformhttpapi.GetManagedAIProductResponseObject, error) {
	product := server.managedProduct
	return platformhttpapi.GetManagedAIProduct200JSONResponse{
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
	request platformhttpapi.IssueAttestationChallengeRequestObject,
) (platformhttpapi.IssueAttestationChallengeResponseObject, error) {
	if server.enrollment == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return platformhttpapi.IssueAttestationChallenge503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	if request.Body == nil {
		failure := newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", "")
		return platformhttpapi.IssueAttestationChallenge422JSONResponse{UnprocessableEntityJSONResponse: platformhttpapi.UnprocessableEntityJSONResponse(failure.platformResponse())}, nil
	}
	challenge, err := server.enrollment.IssueChallenge(ctx, request.Body.KeyID)
	if err != nil {
		if errors.Is(err, attestation.ErrUnavailable) {
			failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
			return platformhttpapi.IssueAttestationChallenge503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
		}
		failure := newAPIFailure(http.StatusUnauthorized, "enrollment_denied", "device enrollment denied", "")
		return platformhttpapi.IssueAttestationChallenge401JSONResponse{UnauthorizedJSONResponse: platformhttpapi.UnauthorizedJSONResponse(failure.platformResponse())}, nil
	}
	return platformhttpapi.IssueAttestationChallenge201JSONResponse{
		Challenge: challenge.Value, ExpiresAt: challenge.ExpiresAt,
	}, nil
}

func (server *Server) RegisterAttestationKey(
	ctx context.Context,
	request platformhttpapi.RegisterAttestationKeyRequestObject,
) (platformhttpapi.RegisterAttestationKeyResponseObject, error) {
	if server.enrollment == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return platformhttpapi.RegisterAttestationKey503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	if request.Body == nil {
		failure := newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", "")
		return platformhttpapi.RegisterAttestationKey422JSONResponse{UnprocessableEntityJSONResponse: platformhttpapi.UnprocessableEntityJSONResponse(failure.platformResponse())}, nil
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
			return platformhttpapi.RegisterAttestationKey503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
		case errors.Is(err, attestation.ErrReplay):
			failure := newAPIFailure(http.StatusConflict, "replay_detected", "registration challenge was already used", "")
			return platformhttpapi.RegisterAttestationKey409JSONResponse{ConflictJSONResponse: platformhttpapi.ConflictJSONResponse(failure.platformResponse())}, nil
		case errors.Is(err, attestation.ErrKeyAlreadyRegistered):
			failure := newAPIFailure(http.StatusConflict, "key_already_registered", "App Attest key is already registered", "")
			return platformhttpapi.RegisterAttestationKey409JSONResponse{ConflictJSONResponse: platformhttpapi.ConflictJSONResponse(failure.platformResponse())}, nil
		default:
			failure := newAPIFailure(http.StatusUnauthorized, "enrollment_denied", "device enrollment denied", "")
			return platformhttpapi.RegisterAttestationKey401JSONResponse{UnauthorizedJSONResponse: platformhttpapi.UnauthorizedJSONResponse(failure.platformResponse())}, nil
		}
	}
	deviceID, err := uuid.Parse(principal.DeviceID)
	if err != nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return platformhttpapi.RegisterAttestationKey503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	return platformhttpapi.RegisterAttestationKey201JSONResponse{KeyID: principal.KeyID, DeviceID: deviceID}, nil
}

func (server *Server) ActivateDevelopmentEntitlement(
	ctx context.Context,
	request platformhttpapi.ActivateDevelopmentEntitlementRequestObject,
) (platformhttpapi.ActivateDevelopmentEntitlementResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.activator == nil {
		failure := newAPIFailure(http.StatusNotFound, "not_found", "route not found", requestID.String())
		return platformhttpapi.ActivateDevelopmentEntitlement404JSONResponse{NotFoundJSONResponse: platformhttpapi.NotFoundJSONResponse(failure.platformResponse())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return platformhttpapi.ActivateDevelopmentEntitlementdefaultJSONResponse{Body: failure.platformResponse(), StatusCode: failure.status}, nil
	}
	expiresAt, err := server.activator.Activate(ctx, principal, request.Params.XTellyouwhatDevActivation)
	if err != nil {
		if errors.Is(err, entitlement.ErrActivationDenied) {
			failure = newAPIFailure(http.StatusForbidden, "activation_denied", "development entitlement activation denied", requestID.String())
			return platformhttpapi.ActivateDevelopmentEntitlement403JSONResponse{ForbiddenJSONResponse: platformhttpapi.ForbiddenJSONResponse(failure.platformResponse())}, nil
		}
		failure = newAPIFailure(http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", requestID.String())
		return platformhttpapi.ActivateDevelopmentEntitlement503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	return platformhttpapi.ActivateDevelopmentEntitlement200JSONResponse{Status: platformhttpapi.Active, ExpiresAt: expiresAt}, nil
}

func (server *Server) SynchronizeSubscriptionTransaction(
	ctx context.Context,
	request platformhttpapi.SynchronizeSubscriptionTransactionRequestObject,
) (platformhttpapi.SynchronizeSubscriptionTransactionResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.productionEntitlement == nil {
		failure := newAPIFailure(http.StatusNotFound, "not_found", "route not found", requestID.String())
		return platformhttpapi.SynchronizeSubscriptionTransaction404JSONResponse{NotFoundJSONResponse: platformhttpapi.NotFoundJSONResponse(failure.platformResponse())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return platformhttpapi.SynchronizeSubscriptionTransactiondefaultJSONResponse{Body: failure.platformResponse(), StatusCode: failure.status}, nil
	}
	if request.Body == nil {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", requestID.String())
		return platformhttpapi.SynchronizeSubscriptionTransaction422JSONResponse{UnprocessableEntityJSONResponse: platformhttpapi.UnprocessableEntityJSONResponse(failure.platformResponse())}, nil
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
		return platformhttpapi.SynchronizeSubscriptionTransactiondefaultJSONResponse{Body: failure.platformResponse(), StatusCode: failure.status}, nil
	}
	return platformhttpapi.SynchronizeSubscriptionTransaction200JSONResponse{Status: platformhttpapi.Active, ExpiresAt: expiresAt}, nil
}

func (server *Server) ProcessAppStoreNotification(
	ctx context.Context,
	request platformhttpapi.ProcessAppStoreNotificationRequestObject,
) (platformhttpapi.ProcessAppStoreNotificationResponseObject, error) {
	if server.appStoreNotifications == nil {
		failure := newAPIFailure(http.StatusNotFound, "not_found", "route not found", "")
		return platformhttpapi.ProcessAppStoreNotification404JSONResponse{NotFoundJSONResponse: platformhttpapi.NotFoundJSONResponse(failure.platformResponse())}, nil
	}
	if request.Body == nil {
		failure := newAPIFailure(http.StatusBadRequest, "invalid_notification_envelope", "notification envelope is invalid", "")
		return platformhttpapi.ProcessAppStoreNotification400JSONResponse{BadRequestJSONResponse: platformhttpapi.BadRequestJSONResponse(failure.platformResponse())}, nil
	}
	if _, err := server.appStoreNotifications.Process(ctx, request.Body.SignedPayload); err != nil {
		if errors.Is(err, entitlement.ErrProductionSyncDenied) {
			failure := newAPIFailure(http.StatusBadRequest, "notification_verification_failed", "notification verification failed", "")
			return platformhttpapi.ProcessAppStoreNotification400JSONResponse{BadRequestJSONResponse: platformhttpapi.BadRequestJSONResponse(failure.platformResponse())}, nil
		}
		failure := newAPIFailure(http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", "")
		return platformhttpapi.ProcessAppStoreNotification503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	return platformhttpapi.ProcessAppStoreNotification200JSONResponse{Status: platformhttpapi.OKStatusStatusOk}, nil
}

func (server *Server) RecordPrivacyConsents(
	ctx context.Context,
	request platformhttpapi.RecordPrivacyConsentsRequestObject,
) (platformhttpapi.RecordPrivacyConsentsResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.privacy == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", requestID.String())
		return platformhttpapi.RecordPrivacyConsents503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return platformhttpapi.RecordPrivacyConsentsdefaultJSONResponse{Body: failure.platformResponse(), StatusCode: failure.status}, nil
	}
	if request.Body == nil {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", requestID.String())
		return platformhttpapi.RecordPrivacyConsents422JSONResponse{UnprocessableEntityJSONResponse: platformhttpapi.UnprocessableEntityJSONResponse(failure.platformResponse())}, nil
	}
	consents := make([]privacy.Consent, 0, len(request.Body.Consents))
	for _, consent := range request.Body.Consents {
		if _, allowed := server.allowedConsentScopes[string(consent.Scope)]; !allowed {
			failure = newAPIFailure(http.StatusUnprocessableEntity, "invalid_consent", "consent scope is not available for this application", requestID.String())
			return platformhttpapi.RecordPrivacyConsents422JSONResponse{UnprocessableEntityJSONResponse: platformhttpapi.UnprocessableEntityJSONResponse(failure.platformResponse())}, nil
		}
		consents = append(consents, privacy.Consent{
			Scope: string(consent.Scope), DocumentVersion: consent.DocumentVersion, Granted: consent.Granted,
		})
	}
	recordedAt, err := server.privacy.RecordConsents(ctx, principal, consents)
	if errors.Is(err, privacy.ErrInvalidConsent) {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "invalid_consent", "consent scope or document version is invalid", requestID.String())
		return platformhttpapi.RecordPrivacyConsents422JSONResponse{UnprocessableEntityJSONResponse: platformhttpapi.UnprocessableEntityJSONResponse(failure.platformResponse())}, nil
	}
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", requestID.String())
		return platformhttpapi.RecordPrivacyConsents503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	return platformhttpapi.RecordPrivacyConsents200JSONResponse{RecordedAt: recordedAt, Count: len(consents)}, nil
}

func (server *Server) DeletePrivacyData(
	ctx context.Context,
	request platformhttpapi.DeletePrivacyDataRequestObject,
) (platformhttpapi.DeletePrivacyDataResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.privacy == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", requestID.String())
		return platformhttpapi.DeletePrivacyData503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return platformhttpapi.DeletePrivacyDatadefaultJSONResponse{Body: failure.platformResponse(), StatusCode: failure.status}, nil
	}
	if err := server.privacy.DeletePrincipal(ctx, principal); err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "deletion_unavailable", "data deletion could not be completed", requestID.String())
		return platformhttpapi.DeletePrivacyData503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	return platformhttpapi.DeletePrivacyData204Response{}, nil
}

func (server *Server) GetAIQuota(
	ctx context.Context,
	request platformhttpapi.GetAIQuotaRequestObject,
) (platformhttpapi.GetAIQuotaResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.quotaReader == nil || server.entitlements == nil ||
		(server.app.ID == appregistry.Health && (server.freeRecognitionQuotaReader == nil || server.recognitionSessions == nil)) {
		failure := newAPIFailure(http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", requestID.String())
		return platformhttpapi.GetAIQuota503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return platformhttpapi.GetAIQuotadefaultJSONResponse{Body: failure.platformResponse(), StatusCode: failure.status}, nil
	}
	managed, failure := server.apiManagedEntitlement(ctx, principal, requestID.String())
	if failure != nil {
		return platformhttpapi.GetAIQuotadefaultJSONResponse{Body: failure.platformResponse(), StatusCode: failure.status}, nil
	}
	reader := server.quotaReader
	plan := platformhttpapi.QuotaSnapshotPlanManagedSubscription
	if !managed {
		if server.app.ID != appregistry.Health || server.freeRecognitionQuotaReader == nil {
			failure = newAPIFailure(http.StatusForbidden, "managed_subscription_required", "managed subscription required", requestID.String())
			return platformhttpapi.GetAIQuota403JSONResponse{ForbiddenJSONResponse: platformhttpapi.ForbiddenJSONResponse(failure.platformResponse())}, nil
		}
		reader = server.freeRecognitionQuotaReader
		plan = platformhttpapi.QuotaSnapshotPlanFree
	}
	quotaPrincipal := principalForQuota(principal, managed)
	snapshot, err := reader.Snapshot(ctx, quotaPrincipal.TransactionID, server.now())
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", requestID.String())
		return platformhttpapi.GetAIQuota503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
	}
	startHour := 4
	if request.Params.BusinessDayStartHour != nil {
		startHour = *request.Params.BusinessDayStartHour
	}
	timeZoneIdentifier := "Asia/Shanghai"
	if request.Params.TimeZoneIdentifier != nil {
		timeZoneIdentifier = *request.Params.TimeZoneIdentifier
	}
	response := platformhttpapi.GetAIQuota200JSONResponse{
		Plan:      plan,
		DailyUsed: snapshot.DailyUsed, DailyLimit: snapshot.DailyLimit, DailyResetAt: snapshot.DailyResetAt,
		MonthlyUsed: snapshot.MonthlyUsed, MonthlyLimit: snapshot.MonthlyLimit, MonthlyResetAt: snapshot.MonthlyResetAt,
	}
	if server.app.ID == appregistry.Health {
		recognition, err := server.recognitionSessions.Snapshot(ctx, principal.DeviceID, recognitionquota.WindowSettings{
			BusinessDayStartHour: startHour, TimeZoneIdentifier: timeZoneIdentifier,
		}, server.now())
		if err != nil {
			failure = newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID.String())
			return platformhttpapi.GetAIQuota503JSONResponse{ServiceUnavailableJSONResponse: platformhttpapi.ServiceUnavailableJSONResponse(failure.platformResponse())}, nil
		}
		response.Recognition = &platformhttpapi.RecognitionQuotaSnapshot{
			Completed: recognition.Completed, Reserved: recognition.Reserved,
			Remaining: recognition.Remaining, ResetAt: recognition.ResetAt,
		}
	}
	return response, nil
}
