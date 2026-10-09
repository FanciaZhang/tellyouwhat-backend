package gateway

import (
	"context"
	"net/http"

	"github.com/tellyouwhat/backend/internal/journalhttpapi"
)

// The canonical contract includes image operations used by journaldevserver.
// That runtime authenticates and intercepts these routes before this gateway.
// Public generation stays closed until a production image runtime is configured.
func (server *Server) CreateJournalIllustration(ctx context.Context, request journalhttpapi.CreateJournalIllustrationRequestObject) (journalhttpapi.CreateJournalIllustrationResponseObject, error) {
	failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "image service is not configured", request.Params.XTellyouwhatRequestID.String())
	return journalhttpapi.CreateJournalIllustrationdefaultJSONResponse{StatusCode: failure.status, Body: journalErrorResponse(failure)}, nil
}

func (server *Server) GetJournalIllustration(ctx context.Context, request journalhttpapi.GetJournalIllustrationRequestObject) (journalhttpapi.GetJournalIllustrationResponseObject, error) {
	failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "image service is not configured", request.Params.XTellyouwhatRequestID.String())
	return journalhttpapi.GetJournalIllustrationdefaultJSONResponse{StatusCode: failure.status, Body: journalErrorResponse(failure)}, nil
}

func (server *Server) CancelJournalIllustration(ctx context.Context, request journalhttpapi.CancelJournalIllustrationRequestObject) (journalhttpapi.CancelJournalIllustrationResponseObject, error) {
	failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "image service is not configured", request.Params.XTellyouwhatRequestID.String())
	return journalhttpapi.CancelJournalIllustrationdefaultJSONResponse{StatusCode: failure.status, Body: journalErrorResponse(failure)}, nil
}

func (server *Server) GetJournalIllustrationResult(ctx context.Context, request journalhttpapi.GetJournalIllustrationResultRequestObject) (journalhttpapi.GetJournalIllustrationResultResponseObject, error) {
	failure := newAPIFailure(http.StatusServiceUnavailable, "not_ready", "image service is not configured", request.Params.XTellyouwhatRequestID.String())
	return journalhttpapi.GetJournalIllustrationResultdefaultJSONResponse{StatusCode: failure.status, Body: journalErrorResponse(failure)}, nil
}
