package gateway

import (
	"context"
	"net/http"

	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
)

func (s *Server) CreateJournalVoiceSession(ctx context.Context, request journalhttpapi.CreateJournalVoiceSessionRequestObject) (journalhttpapi.CreateJournalVoiceSessionResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	deny := func(failure *apiFailure) (journalhttpapi.CreateJournalVoiceSessionResponseObject, error) {
		return journalhttpapi.CreateJournalVoiceSessiondefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	fail := func(status int, code, message string) (journalhttpapi.CreateJournalVoiceSessionResponseObject, error) {
		return deny(newAPIFailure(status, code, message, requestID.String()))
	}
	if s.voice == nil || s.voiceEntitlements == nil {
		return fail(503, "voice_not_enabled", "voice service is not enabled")
	}
	principal, failure := s.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return deny(failure)
	}
	if failure = s.apiRequireManagedEntitlement(ctx, principal, requestID.String()); failure != nil {
		return deny(failure)
	}
	if failure = s.apiRequireConsents(ctx, principal, s.requiredConsentScopes, requestID.String()); failure != nil {
		return deny(failure)
	}
	if request.Body == nil || request.Body.ConsentVersion != voice.Version {
		return fail(422, "voice_consent_required", "voice consent is required")
	}
	record, ok, err := s.voiceEntitlements.Get(ctx, principal.KeyID)
	if err != nil || !ok || !record.ExpiresAt.After(s.now()) {
		return fail(403, "managed_subscription_required", "subscription required")
	}
	if record.StartedAt.IsZero() {
		return fail(409, "voice_subscription_sync_required", "restore the subscription to synchronize the purchase date")
	}
	owner := record.TransactionID
	if owner == "" && record.Environment == "development" {
		owner = principal.KeyID
	}
	if owner == "" {
		return fail(403, "managed_subscription_required", "subscription required")
	}
	ticket, err := s.voice.Issue(ctx, voice.Identity{Owner: record.Environment + ":" + owner, KeyID: principal.KeyID, Anchor: record.StartedAt, ExpiresAt: record.ExpiresAt}, request.Body.SessionID.String())
	if err != nil {
		return fail(503, "voice_unavailable", "voice session could not be created")
	}
	return journalhttpapi.CreateJournalVoiceSession201JSONResponse{
		SessionID: request.Body.SessionID, Token: ticket.Token,
		RemainingMilliseconds: ticket.RemainingMilliseconds, MaximumMilliseconds: ticket.MaximumMilliseconds,
		ResetsAt: ticket.ResetsAt,
	}, nil
}

func (s *Server) StreamJournalVoiceSession(ctx context.Context, request journalhttpapi.StreamJournalVoiceSessionRequestObject) (journalhttpapi.StreamJournalVoiceSessionResponseObject, error) {
	if s.voice == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "voice_not_enabled", "voice service is not enabled", "")
		return journalhttpapi.StreamJournalVoiceSessiondefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	c := strictGinContext(ctx)
	s.voice.Serve(c.Writer, c.Request, request.SessionID.String())
	// Serve owns the HTTP upgrade and frames. The generated strict adapter must
	// not attempt a second response after the connection has been hijacked.
	return nil, nil
}
