package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/contracts"
	journalcontracts "github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/usage"
)

var _ journalhttpapi.StrictServerInterface = (*Server)(nil)

func journalErrorResponse(failure *apiFailure) journalhttpapi.ErrorResponse {
	return journalhttpapi.ErrorResponse{Error: journalhttpapi.ErrorDetail{
		Code: failure.code, Message: failure.message, RequestID: failure.requestID,
	}}
}

func (server *Server) OrganizeJournal(
	ctx context.Context,
	request journalhttpapi.OrganizeJournalRequestObject,
) (journalhttpapi.OrganizeJournalResponseObject, error) {
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
	if err := server.media.Consume(ctx, principal, artifact, contracts.BodySHA256(rawRequestBody(strictGinContext(ctx)))); err != nil {
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
