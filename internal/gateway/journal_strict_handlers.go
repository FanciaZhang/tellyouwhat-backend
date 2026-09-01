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
	journalcontracts "github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/usage"
)

type journalStrictServer struct {
	server *Server
}

var _ journalhttpapi.StrictServerInterface = (*journalStrictServer)(nil)

func journalErrorResponse(failure *apiFailure) journalhttpapi.ErrorResponse {
	return journalhttpapi.ErrorResponse{Error: journalhttpapi.ErrorDetail{
		Code: failure.code, Message: failure.message, RequestID: failure.requestID,
	}}
}

func (adapter *journalStrictServer) GetHealth(
	context.Context,
	journalhttpapi.GetHealthRequestObject,
) (journalhttpapi.GetHealthResponseObject, error) {
	return journalhttpapi.GetHealth200JSONResponse{Status: journalhttpapi.ServiceStatusStatusOk}, nil
}

func (adapter *journalStrictServer) GetReadiness(
	ctx context.Context,
	_ journalhttpapi.GetReadinessRequestObject,
) (journalhttpapi.GetReadinessResponseObject, error) {
	server := adapter.server
	unavailable := server.authenticator == nil || server.entitlements == nil || server.quota == nil || server.quotaReader == nil ||
		server.enrollment == nil || server.usage == nil || server.readiness == nil || server.privacy == nil || server.consent == nil ||
		server.journalOrganizer == nil || server.journalAnalysisVersion == "" || server.media == nil
	if unavailable {
		failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", "")
		return journalhttpapi.GetReadiness503JSONResponse{
			ServiceUnavailableJSONResponse: journalhttpapi.ServiceUnavailableJSONResponse(journalErrorResponse(failure)),
		}, nil
	}
	readinessContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := server.readiness.Ready(readinessContext); err != nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", "")
		return journalhttpapi.GetReadiness503JSONResponse{
			ServiceUnavailableJSONResponse: journalhttpapi.ServiceUnavailableJSONResponse(journalErrorResponse(failure)),
		}, nil
	}
	return journalhttpapi.GetReadiness200JSONResponse{Status: journalhttpapi.ServiceStatusStatusReady}, nil
}

func (adapter *journalStrictServer) GetManagedAIProduct(
	context.Context,
	journalhttpapi.GetManagedAIProductRequestObject,
) (journalhttpapi.GetManagedAIProductResponseObject, error) {
	product := adapter.server.managedProduct
	return journalhttpapi.GetManagedAIProduct200JSONResponse{
		ProductID: product.ProductID, BillingPeriod: product.BillingPeriod,
		DailyTokenLimit: product.DailyTokenLimit, MonthlyTokenLimit: product.MonthlyTokenLimit,
		Provider: product.Provider, ModelDisclosure: product.ModelDisclosure,
		MediaRetention: product.MediaRetention, JobRetention: product.JobRetention,
		PrivacyURL: product.PrivacyURL, TermsURL: product.TermsURL,
		PrivacyChoicesURL: product.PrivacyChoicesURL, SupportURL: product.SupportURL,
	}, nil
}

func (adapter *journalStrictServer) IssueAttestationChallenge(
	ctx context.Context,
	request journalhttpapi.IssueAttestationChallengeRequestObject,
) (journalhttpapi.IssueAttestationChallengeResponseObject, error) {
	server := adapter.server
	if server.enrollment == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return journalhttpapi.IssueAttestationChallengedefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if request.Body == nil {
		failure := newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", "")
		return journalhttpapi.IssueAttestationChallengedefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	challenge, err := server.enrollment.IssueChallenge(ctx, request.Body.KeyID)
	if err != nil {
		if errors.Is(err, attestation.ErrUnavailable) {
			failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
			return journalhttpapi.IssueAttestationChallengedefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
		}
		failure := newAPIFailure(http.StatusUnauthorized, "enrollment_denied", "device enrollment denied", "")
		return journalhttpapi.IssueAttestationChallengedefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.IssueAttestationChallenge201JSONResponse{
		Challenge: challenge.Value, ExpiresAt: challenge.ExpiresAt,
	}, nil
}

func (adapter *journalStrictServer) RegisterAttestationKey(
	ctx context.Context,
	request journalhttpapi.RegisterAttestationKeyRequestObject,
) (journalhttpapi.RegisterAttestationKeyResponseObject, error) {
	server := adapter.server
	if server.enrollment == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return journalhttpapi.RegisterAttestationKeydefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if request.Body == nil {
		failure := newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", "")
		return journalhttpapi.RegisterAttestationKeydefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	principal, err := server.enrollment.Register(ctx, attestation.RegistrationRequest{
		KeyID: request.Body.KeyID, Challenge: request.Body.Challenge,
		Attestation: base64.StdEncoding.EncodeToString(request.Body.Attestation),
		Build:       request.Body.Build, ActivationSecret: request.Body.ActivationSecret,
	})
	if err != nil {
		var failure *apiFailure
		switch {
		case errors.Is(err, attestation.ErrUnavailable):
			failure = newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		case errors.Is(err, attestation.ErrReplay):
			failure = newAPIFailure(http.StatusConflict, "replay_detected", "registration challenge was already used", "")
		case errors.Is(err, attestation.ErrKeyAlreadyRegistered):
			failure = newAPIFailure(http.StatusConflict, "key_already_registered", "App Attest key is already registered", "")
		default:
			failure = newAPIFailure(http.StatusUnauthorized, "enrollment_denied", "device enrollment denied", "")
		}
		return journalhttpapi.RegisterAttestationKeydefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	deviceID, err := uuid.Parse(principal.DeviceID)
	if err != nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return journalhttpapi.RegisterAttestationKeydefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.RegisterAttestationKey201JSONResponse{KeyID: principal.KeyID, DeviceID: deviceID}, nil
}

func (adapter *journalStrictServer) ActivateDevelopmentEntitlement(
	ctx context.Context,
	request journalhttpapi.ActivateDevelopmentEntitlementRequestObject,
) (journalhttpapi.ActivateDevelopmentEntitlementResponseObject, error) {
	server := adapter.server
	requestID := request.Params.XTellyouwhatRequestID
	if server.activator == nil {
		failure := newAPIFailure(http.StatusNotFound, "not_found", "route not found", requestID.String())
		return journalhttpapi.ActivateDevelopmentEntitlementdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return journalhttpapi.ActivateDevelopmentEntitlementdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	expiresAt, err := server.activator.Activate(ctx, principal, request.Params.XTellyouwhatDevActivation)
	if err != nil {
		if errors.Is(err, entitlement.ErrActivationDenied) {
			failure = newAPIFailure(http.StatusForbidden, "activation_denied", "development entitlement activation denied", requestID.String())
		} else {
			failure = newAPIFailure(http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", requestID.String())
		}
		return journalhttpapi.ActivateDevelopmentEntitlementdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.ActivateDevelopmentEntitlement200JSONResponse{Status: journalhttpapi.Active, ExpiresAt: expiresAt}, nil
}

func (adapter *journalStrictServer) SynchronizeSubscriptionTransaction(
	ctx context.Context,
	request journalhttpapi.SynchronizeSubscriptionTransactionRequestObject,
) (journalhttpapi.SynchronizeSubscriptionTransactionResponseObject, error) {
	server := adapter.server
	requestID := request.Params.XTellyouwhatRequestID
	if server.productionEntitlement == nil {
		failure := newAPIFailure(http.StatusNotFound, "not_found", "route not found", requestID.String())
		return journalhttpapi.SynchronizeSubscriptionTransactiondefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return journalhttpapi.SynchronizeSubscriptionTransactiondefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if request.Body == nil {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", requestID.String())
		return journalhttpapi.SynchronizeSubscriptionTransactiondefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
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
		return journalhttpapi.SynchronizeSubscriptionTransactiondefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.SynchronizeSubscriptionTransaction200JSONResponse{Status: journalhttpapi.Active, ExpiresAt: expiresAt}, nil
}

func (adapter *journalStrictServer) ProcessAppStoreNotification(
	ctx context.Context,
	request journalhttpapi.ProcessAppStoreNotificationRequestObject,
) (journalhttpapi.ProcessAppStoreNotificationResponseObject, error) {
	server := adapter.server
	if server.appStoreNotifications == nil {
		failure := newAPIFailure(http.StatusNotFound, "not_found", "route not found", "")
		return journalhttpapi.ProcessAppStoreNotificationdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if request.Body == nil {
		failure := newAPIFailure(http.StatusBadRequest, "invalid_notification_envelope", "notification envelope is invalid", "")
		return journalhttpapi.ProcessAppStoreNotificationdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if _, err := server.appStoreNotifications.Process(ctx, request.Body.SignedPayload); err != nil {
		if errors.Is(err, entitlement.ErrProductionSyncDenied) {
			failure := newAPIFailure(http.StatusBadRequest, "notification_verification_failed", "notification verification failed", "")
			return journalhttpapi.ProcessAppStoreNotificationdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
		}
		failure := newAPIFailure(http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", "")
		return journalhttpapi.ProcessAppStoreNotificationdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.ProcessAppStoreNotification200JSONResponse{Status: journalhttpapi.OKStatusStatusOk}, nil
}

func (adapter *journalStrictServer) RecordPrivacyConsents(
	ctx context.Context,
	request journalhttpapi.RecordPrivacyConsentsRequestObject,
) (journalhttpapi.RecordPrivacyConsentsResponseObject, error) {
	server := adapter.server
	requestID := request.Params.XTellyouwhatRequestID
	if server.privacy == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", requestID.String())
		return journalhttpapi.RecordPrivacyConsentsdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return journalhttpapi.RecordPrivacyConsentsdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if request.Body == nil {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", requestID.String())
		return journalhttpapi.RecordPrivacyConsentsdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if len(request.Body.Consents) != 1 || request.Body.Consents[0].Scope != journalhttpapi.ManagedSubscription {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "invalid_consent", "consent scope or document version is invalid", requestID.String())
		return journalhttpapi.RecordPrivacyConsentsdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
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
		return journalhttpapi.RecordPrivacyConsentsdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", requestID.String())
		return journalhttpapi.RecordPrivacyConsentsdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.RecordPrivacyConsents200JSONResponse{RecordedAt: recordedAt, Count: len(consents)}, nil
}

func (adapter *journalStrictServer) DeletePrivacyData(
	ctx context.Context,
	request journalhttpapi.DeletePrivacyDataRequestObject,
) (journalhttpapi.DeletePrivacyDataResponseObject, error) {
	server := adapter.server
	requestID := request.Params.XTellyouwhatRequestID
	if server.privacy == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", requestID.String())
		return journalhttpapi.DeletePrivacyDatadefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return journalhttpapi.DeletePrivacyDatadefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if err := server.privacy.DeletePrincipal(ctx, principal); err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "deletion_unavailable", "data deletion could not be completed", requestID.String())
		return journalhttpapi.DeletePrivacyDatadefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.DeletePrivacyData204Response{}, nil
}

func (adapter *journalStrictServer) GetAIQuota(
	ctx context.Context,
	request journalhttpapi.GetAIQuotaRequestObject,
) (journalhttpapi.GetAIQuotaResponseObject, error) {
	server := adapter.server
	requestID := request.Params.XTellyouwhatRequestID
	if server.quotaReader == nil || server.entitlements == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", requestID.String())
		return journalhttpapi.GetAIQuotadefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return journalhttpapi.GetAIQuotadefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if failure = server.apiRequireManagedEntitlement(ctx, principal, requestID.String()); failure != nil {
		return journalhttpapi.GetAIQuotadefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	transactionID := principal.TransactionID
	if transactionID == "" {
		transactionID = principal.KeyID
	}
	snapshot, err := server.quotaReader.Snapshot(ctx, transactionID, server.now())
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", requestID.String())
		return journalhttpapi.GetAIQuotadefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.GetAIQuota200JSONResponse{
		DailyUsed: snapshot.DailyUsed, DailyLimit: snapshot.DailyLimit, DailyResetAt: snapshot.DailyResetAt,
		MonthlyUsed: snapshot.MonthlyUsed, MonthlyLimit: snapshot.MonthlyLimit, MonthlyResetAt: snapshot.MonthlyResetAt,
	}, nil
}

func (adapter *journalStrictServer) OrganizeJournal(
	ctx context.Context,
	request journalhttpapi.OrganizeJournalRequestObject,
) (journalhttpapi.OrganizeJournalResponseObject, error) {
	server := adapter.server
	requestID := request.Params.XTellyouwhatRequestID
	if server.journalOrganizer == nil || server.journalAnalysisVersion == "" || server.entitlements == nil || server.quota == nil || server.quotaReader == nil || server.media == nil || server.usage == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", requestID.String())
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	input, failure := journalOrganizeRequest(request.Body, requestID)
	if failure != nil {
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if !server.app.AllowsOperation("journal.organize") {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "operation is not available for this application", requestID.String())
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if failure = server.apiRequireManagedEntitlement(ctx, principal, requestID.String()); failure != nil {
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if failure = server.apiRequireConsents(ctx, principal, server.requiredConsentScopes, requestID.String()); failure != nil {
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	transactionID := principal.TransactionID
	if transactionID == "" {
		transactionID = principal.KeyID
	}
	estimatedTokens := journalReservationTokens(input)
	lease, err := server.quota.Acquire(ctx, quota.Identity{
		DeviceID: principal.DeviceID, TransactionID: transactionID,
		IP: server.ipResolver(strictGinContext(ctx).Request),
	}, contracts.Operation("journal.organize"), estimatedTokens, "", server.now())
	if err != nil {
		if errors.Is(err, quota.ErrExceeded) {
			code, message := journalQuotaExceededResponse(err)
			failure = newAPIFailure(http.StatusTooManyRequests, code, message, requestID.String())
		} else {
			failure = newAPIFailure(http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", requestID.String())
		}
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	artifact := contracts.Request{RequestID: input.RequestID, Operation: contracts.Operation("journal.organize")}
	if err := server.media.Consume(ctx, principal, artifact, contracts.BodySHA256(journalRawRequestBody(strictGinContext(ctx)))); err != nil {
		lease.Release(0)
		failure = server.apiAdmissionFailure(err, input.RequestID)
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	result, err := server.journalOrganizer.Organize(ctx, input)
	if err != nil {
		lease.Release(0)
		failure = newAPIFailure(http.StatusBadGateway, "upstream_error", "managed AI provider failed", input.RequestID)
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	actualTokens := result.InputTokens + result.OutputTokens
	if err := server.usage.Record(ctx, usage.Record{
		RequestID: input.RequestID, KeyID: principal.KeyID, DeviceID: principal.DeviceID,
		TransactionID: transactionID, Operation: contracts.Operation("journal.organize"),
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens, OccurredAt: server.now(),
	}); err != nil {
		actualTokens = estimatedTokens
	}
	lease.Release(actualTokens)
	snapshot, snapshotErr := server.quotaReader.Snapshot(ctx, transactionID, server.now())
	response := journalcontracts.OrganizeResponse{
		RequestID: input.RequestID, ContentHash: input.ContentHash,
		AnalysisVersion:             server.journalAnalysisVersion,
		Tags:                        result.Value.Tags,
		ExistingBookRecommendations: result.Value.ExistingBookRecommendations,
		NewBookSuggestions:          result.Value.NewBookSuggestions,
		Quota: journalcontracts.Quota{
			DailyTokensRemaining:   max(0, snapshot.DailyLimit-snapshot.DailyUsed),
			MonthlyTokensRemaining: max(0, snapshot.MonthlyLimit-snapshot.MonthlyUsed),
			Available:              snapshotErr == nil,
		},
	}
	bookIDs := make(map[string]bool, len(input.Books))
	for _, book := range input.Books {
		bookIDs[book.ID] = true
	}
	if err := response.Validate(bookIDs); err != nil {
		failure = newAPIFailure(http.StatusBadGateway, "invalid_model_result", "managed AI returned an invalid result", input.RequestID)
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	output, err := journalOrganizeResponse(response, requestID)
	if err != nil {
		failure = newAPIFailure(http.StatusBadGateway, "invalid_model_result", "managed AI returned an invalid result", input.RequestID)
		return journalhttpapi.OrganizeJournaldefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.OrganizeJournal200JSONResponse(output), nil
}

func journalOrganizeRequest(body *journalhttpapi.OrganizeRequest, requestID uuid.UUID) (journalcontracts.OrganizeRequest, *apiFailure) {
	if body == nil {
		return journalcontracts.OrganizeRequest{}, newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request body is required", requestID.String())
	}
	if body.RequestID != requestID {
		return journalcontracts.OrganizeRequest{}, newAPIFailure(http.StatusUnauthorized, "authentication_failed", "request authentication failed", requestID.String())
	}
	input := journalcontracts.OrganizeRequest{
		RequestID: body.RequestID.String(), ContractVersion: string(body.ContractVersion),
		ContentHash: body.ContentHash, Title: body.Title, Body: body.Body,
		ExistingTags:     append([]string(nil), body.ExistingTags...),
		RejectedTagNames: append([]string(nil), body.RejectedTagNames...),
		Books:            make([]journalcontracts.BookContext, 0, len(body.Books)),
	}
	for _, book := range body.Books {
		input.Books = append(input.Books, journalcontracts.BookContext{
			ID: book.Id.String(), Name: book.Name, Description: book.Description, ContainsEntry: book.ContainsEntry,
		})
	}
	if err := input.Validate(); err != nil {
		return journalcontracts.OrganizeRequest{}, newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request violates the journal organization contract", requestID.String())
	}
	return input, nil
}

func journalOrganizeResponse(response journalcontracts.OrganizeResponse, requestID uuid.UUID) (journalhttpapi.OrganizeResponse, error) {
	output := journalhttpapi.OrganizeResponse{
		RequestID: requestID, ContentHash: response.ContentHash, AnalysisVersion: response.AnalysisVersion,
		Tags:                        make([]journalhttpapi.Tag, 0, len(response.Tags)),
		ExistingBookRecommendations: make([]journalhttpapi.ExistingBookRecommendation, 0, len(response.ExistingBookRecommendations)),
		NewBookSuggestions:          make([]journalhttpapi.NewBookSuggestion, 0, len(response.NewBookSuggestions)),
		Quota: journalhttpapi.OrganizeQuota{
			DailyTokensRemaining:   response.Quota.DailyTokensRemaining,
			MonthlyTokensRemaining: response.Quota.MonthlyTokensRemaining,
			Available:              response.Quota.Available,
		},
	}
	for _, tag := range response.Tags {
		output.Tags = append(output.Tags, journalhttpapi.Tag{Name: tag.Name, Type: journalhttpapi.TagType(tag.Type)})
	}
	for _, recommendation := range response.ExistingBookRecommendations {
		bookID, err := uuid.Parse(recommendation.BookID)
		if err != nil {
			return journalhttpapi.OrganizeResponse{}, err
		}
		output.ExistingBookRecommendations = append(output.ExistingBookRecommendations, journalhttpapi.ExistingBookRecommendation{
			BookID: bookID, Reason: recommendation.Reason,
		})
	}
	for _, suggestion := range response.NewBookSuggestions {
		output.NewBookSuggestions = append(output.NewBookSuggestions, journalhttpapi.NewBookSuggestion{
			Name: suggestion.Name, Description: suggestion.Description, Reason: suggestion.Reason,
			RelatedTags: append([]string(nil), suggestion.RelatedTags...),
		})
	}
	return output, nil
}
