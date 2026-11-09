package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/healthhttpapi"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/recognitionquota"
)

var _ healthhttpapi.StrictServerInterface = (*Server)(nil)

func (server *Server) AuthorizeMediaUpload(
	ctx context.Context,
	request healthhttpapi.AuthorizeMediaUploadRequestObject,
) (healthhttpapi.AuthorizeMediaUploadResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.media == nil || server.entitlements == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "media_unavailable", "media service unavailable", requestID.String())
		return healthhttpapi.AuthorizeMediaUpload503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return healthhttpapi.AuthorizeMediaUploaddefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if request.Body == nil || request.Body.RequestID != requestID {
		failure = newAPIFailure(http.StatusUnauthorized, "authentication_failed", "request authentication failed", requestID.String())
		return healthhttpapi.AuthorizeMediaUpload401JSONResponse{UnauthorizedJSONResponse: healthhttpapi.UnauthorizedJSONResponse(healthErrorResponse(failure))}, nil
	}
	artifact := contracts.Request{Operation: contracts.Operation(request.Body.Operation)}
	if !server.app.AllowsOperation(string(artifact.Operation)) {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "operation is not available for this application", requestID.String())
		return healthhttpapi.AuthorizeMediaUpload422JSONResponse{UnprocessableEntityJSONResponse: healthhttpapi.UnprocessableEntityJSONResponse(healthErrorResponse(failure))}, nil
	}
	if request.Body.RecognitionSession != nil {
		artifact.RecognitionSession = &contracts.RecognitionSessionContext{
			SessionID:            request.Body.RecognitionSession.SessionID.String(),
			BusinessDayStartHour: request.Body.RecognitionSession.BusinessDayStartHour,
			TimeZoneIdentifier:   request.Body.RecognitionSession.TimeZoneIdentifier,
		}
	}
	if _, failure = server.apiAuthorizeAIEntitlement(ctx, principal, artifact, requestID.String()); failure != nil {
		return healthhttpapi.AuthorizeMediaUploaddefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
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
		return healthhttpapi.AuthorizeMediaUploaddefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return healthhttpapi.AuthorizeMediaUpload201JSONResponse{
		ObjectID: authorization.ObjectID, UploadURL: authorization.UploadURL,
		RequiredHeaders: authorization.RequiredHeaders, ExpiresAt: authorization.ExpiresAt,
	}, nil
}

func (server *Server) CompleteRecognitionSession(
	ctx context.Context,
	request healthhttpapi.CompleteRecognitionSessionRequestObject,
) (healthhttpapi.CompleteRecognitionSessionResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.recognitionSessions == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID.String())
		return healthhttpapi.CompleteRecognitionSession503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return healthhttpapi.CompleteRecognitionSessiondefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	snapshot, err := server.recognitionSessions.Complete(ctx, principal.DeviceID, request.Id.String(), server.now())
	if errors.Is(err, recognitionquota.ErrNotFound) {
		failure = newAPIFailure(http.StatusNotFound, "recognition_session_not_found", "recognition session was not found or has expired", requestID.String())
		return healthhttpapi.CompleteRecognitionSession404JSONResponse{NotFoundJSONResponse: healthhttpapi.NotFoundJSONResponse(healthErrorResponse(failure))}, nil
	}
	if errors.Is(err, recognitionquota.ErrInvalid) {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "invalid_recognition_session", "recognition session is invalid", requestID.String())
		return healthhttpapi.CompleteRecognitionSession422JSONResponse{UnprocessableEntityJSONResponse: healthhttpapi.UnprocessableEntityJSONResponse(healthErrorResponse(failure))}, nil
	}
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID.String())
		return healthhttpapi.CompleteRecognitionSession503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	return healthhttpapi.CompleteRecognitionSession200JSONResponse{
		Completed: snapshot.Completed, Reserved: snapshot.Reserved,
		Remaining: snapshot.Remaining, ResetAt: snapshot.ResetAt,
	}, nil
}

func (server *Server) CancelRecognitionSession(
	ctx context.Context,
	request healthhttpapi.CancelRecognitionSessionRequestObject,
) (healthhttpapi.CancelRecognitionSessionResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.recognitionSessions == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID.String())
		return healthhttpapi.CancelRecognitionSession503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return healthhttpapi.CancelRecognitionSessiondefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if err := server.recognitionSessions.Cancel(ctx, principal.DeviceID, request.Id.String(), server.now()); err != nil {
		if errors.Is(err, recognitionquota.ErrInvalid) {
			failure = newAPIFailure(http.StatusUnprocessableEntity, "invalid_recognition_session", "recognition session is invalid", requestID.String())
			return healthhttpapi.CancelRecognitionSession422JSONResponse{UnprocessableEntityJSONResponse: healthhttpapi.UnprocessableEntityJSONResponse(healthErrorResponse(failure))}, nil
		}
		failure = newAPIFailure(http.StatusServiceUnavailable, "recognition_quota_unavailable", "free recognition quota unavailable", requestID.String())
		return healthhttpapi.CancelRecognitionSession503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	return healthhttpapi.CancelRecognitionSession204Response{}, nil
}
