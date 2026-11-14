package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"github.com/tellyouwhat/backend/internal/privacy"
)

func (s *Server) organizationPrincipal(ctx context.Context, id uuid.UUID, cancellation bool) (Principal, *apiFailure) {
	if s.journalVoiceOrganizations == nil {
		return Principal{}, newAPIFailure(503, "not_ready", "voice organization is not configured", id.String())
	}
	p, f := s.apiAuthenticate(ctx, id)
	if f != nil || cancellation {
		return p, f
	}
	if f = s.apiRequireManagedEntitlement(ctx, p, id.String()); f != nil {
		return Principal{}, f
	}
	if f = s.apiRequireConsents(ctx, p, []string{privacy.ManagedAIScope}, id.String()); f != nil {
		return Principal{}, f
	}
	if f = s.operationsFailure(ctx, "journal.voice.rewrite", id.String()); f != nil {
		return Principal{}, f
	}
	return p, nil
}

func organizationFailure(err error, id string) *apiFailure {
	switch {
	case errors.Is(err, voice.ErrOrganizationNotFound):
		return newAPIFailure(404, "voice_organization_not_found", "organization task not found", id)
	case errors.Is(err, voice.ErrOrganizationExpired):
		return newAPIFailure(410, "voice_organization_expired", "organization task expired", id)
	case errors.Is(err, voice.ErrOrganizationConflict):
		return newAPIFailure(409, "voice_organization_changed", "organization task changed", id)
	case errors.Is(err, voice.ErrInvalid):
		return newAPIFailure(422, "contract_violation", "invalid organization request", id)
	default:
		return newAPIFailure(503, "voice_organization_unavailable", "organization unavailable", id)
	}
}

func organizationResponse(job voice.OrganizationJob) (journalhttpapi.VoiceOrganizationTask, error) {
	id, err := uuid.Parse(job.ID)
	if err != nil {
		return journalhttpapi.VoiceOrganizationTask{}, err
	}
	result := journalhttpapi.VoiceOrganizationTask{Id: id, InputSHA256: job.Digest, State: journalhttpapi.VoiceOrganizationTaskState(job.State), Revision: int64(job.Revision), ExpiresAt: job.ExpiresAt}
	if job.Problem != "" {
		problem := job.Problem
		result.Problem = &problem
	}
	if job.State == "succeeded" {
		data, err := json.Marshal(job.Progress.Results)
		if err != nil {
			return journalhttpapi.VoiceOrganizationTask{}, err
		}
		result.Result = &data
	}
	return result, nil
}

func (s *Server) CreateJournalVoiceOrganization(ctx context.Context, r journalhttpapi.CreateJournalVoiceOrganizationRequestObject) (journalhttpapi.CreateJournalVoiceOrganizationResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	strictGinContext(ctx).Header("Cache-Control", "no-store")
	deny := func(f *apiFailure) (journalhttpapi.CreateJournalVoiceOrganizationResponseObject, error) {
		return journalhttpapi.CreateJournalVoiceOrganizationdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.organizationPrincipal(ctx, id, false)
	if f != nil {
		return deny(f)
	}
	if r.Body == nil || r.Body.RequestID != id || id == uuid.Nil {
		return deny(organizationFailure(voice.ErrInvalid, id.String()))
	}
	job, err := s.journalVoiceOrganizations.Create(apiRequestContext(ctx), p, id.String(), r.Body.InputSHA256, r.Body.Plan, s.ipResolver(strictGinContext(ctx).Request))
	if err != nil {
		return deny(organizationFailure(err, id.String()))
	}
	body, err := organizationResponse(job)
	if err != nil {
		return deny(organizationFailure(err, id.String()))
	}
	strictGinContext(ctx).Header("Cache-Control", "no-store")
	return journalhttpapi.CreateJournalVoiceOrganization200JSONResponse(body), nil
}

func (s *Server) GetJournalVoiceOrganization(ctx context.Context, r journalhttpapi.GetJournalVoiceOrganizationRequestObject) (journalhttpapi.GetJournalVoiceOrganizationResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	strictGinContext(ctx).Header("Cache-Control", "no-store")
	deny := func(f *apiFailure) (journalhttpapi.GetJournalVoiceOrganizationResponseObject, error) {
		return journalhttpapi.GetJournalVoiceOrganizationdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.organizationPrincipal(ctx, id, false)
	if f != nil {
		return deny(f)
	}
	job, err := s.journalVoiceOrganizations.Get(apiRequestContext(ctx), p.KeyID, r.TaskID.String())
	if err != nil {
		return deny(organizationFailure(err, id.String()))
	}
	body, err := organizationResponse(job)
	if err != nil {
		return deny(organizationFailure(err, id.String()))
	}
	strictGinContext(ctx).Header("Cache-Control", "no-store")
	return journalhttpapi.GetJournalVoiceOrganization200JSONResponse(body), nil
}

func (s *Server) RetryJournalVoiceOrganization(ctx context.Context, r journalhttpapi.RetryJournalVoiceOrganizationRequestObject) (journalhttpapi.RetryJournalVoiceOrganizationResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	strictGinContext(ctx).Header("Cache-Control", "no-store")
	deny := func(f *apiFailure) (journalhttpapi.RetryJournalVoiceOrganizationResponseObject, error) {
		return journalhttpapi.RetryJournalVoiceOrganizationdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.organizationPrincipal(ctx, id, false)
	if f != nil {
		return deny(f)
	}
	if r.Body == nil || r.Body.Revision < 1 {
		return deny(organizationFailure(voice.ErrInvalid, id.String()))
	}
	job, err := s.journalVoiceOrganizations.Retry(apiRequestContext(ctx), p.KeyID, r.TaskID.String(), uint64(r.Body.Revision))
	if err != nil {
		return deny(organizationFailure(err, id.String()))
	}
	body, err := organizationResponse(job)
	if err != nil {
		return deny(organizationFailure(err, id.String()))
	}
	return journalhttpapi.RetryJournalVoiceOrganization200JSONResponse(body), nil
}

func (s *Server) CancelJournalVoiceOrganization(ctx context.Context, r journalhttpapi.CancelJournalVoiceOrganizationRequestObject) (journalhttpapi.CancelJournalVoiceOrganizationResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	strictGinContext(ctx).Header("Cache-Control", "no-store")
	deny := func(f *apiFailure) (journalhttpapi.CancelJournalVoiceOrganizationResponseObject, error) {
		return journalhttpapi.CancelJournalVoiceOrganizationdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.organizationPrincipal(ctx, id, true)
	if f != nil {
		return deny(f)
	}
	if r.Body == nil || r.Body.Revision < 1 {
		return deny(organizationFailure(voice.ErrInvalid, id.String()))
	}
	job, err := s.journalVoiceOrganizations.Cancel(apiRequestContext(ctx), p, r.TaskID.String(), uint64(r.Body.Revision))
	if err != nil {
		return deny(organizationFailure(err, id.String()))
	}
	body, err := organizationResponse(job)
	if err != nil {
		return deny(organizationFailure(err, id.String()))
	}
	return journalhttpapi.CancelJournalVoiceOrganization200JSONResponse(body), nil
}
