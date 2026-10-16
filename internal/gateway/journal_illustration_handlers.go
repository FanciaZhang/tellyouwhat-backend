package gateway

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"github.com/tellyouwhat/backend/internal/privacy"
)

func (s *Server) imagePrincipal(ctx context.Context, id uuid.UUID, cancellation bool) (Principal, *apiFailure) {
	if s.journalIllustrations == nil {
		return Principal{}, newAPIFailure(503, "not_ready", "image service is not configured", id.String())
	}
	p, f := s.apiAuthenticate(ctx, id)
	if f != nil || cancellation {
		return p, f
	}
	if f = s.apiRequireManagedEntitlement(ctx, p, id.String()); f != nil {
		return Principal{}, f
	}
	if f = s.apiRequireConsents(ctx, p, []string{privacy.ManagedAIScope, privacy.JournalIllustrationScope}, id.String()); f != nil {
		return Principal{}, f
	}
	return p, nil
}

func imageTaskResponse(job illustration.Job) journalhttpapi.IllustrationTask {
	id, _ := uuid.Parse(job.Task.ID)
	version, _ := uuid.Parse(job.Task.VersionID)
	result := journalhttpapi.IllustrationTask{Id: id, VersionID: version, State: journalhttpapi.IllustrationTaskState(job.Task.State), Revision: int64(job.Task.Revision), ExpiresAt: job.Task.ExpiresAt}
	if job.Problem != "" {
		problem := journalhttpapi.IllustrationTaskProblem(job.Problem)
		result.Problem = &problem
	}
	return result
}

func imageFailure(err error, id string) *apiFailure {
	switch {
	case errors.Is(err, illustration.ErrNotFound):
		return newAPIFailure(404, "not_found", "image task not found", id)
	case errors.Is(err, illustration.ErrInput):
		return newAPIFailure(422, "contract_violation", "invalid image request", id)
	case errors.Is(err, illustration.ErrTaskConflict):
		return newAPIFailure(409, "image_task_changed", "image task changed", id)
	default:
		return newAPIFailure(503, "image_unavailable", "image service unavailable", id)
	}
}

func (s *Server) CreateJournalIllustration(ctx context.Context, r journalhttpapi.CreateJournalIllustrationRequestObject) (journalhttpapi.CreateJournalIllustrationResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	deny := func(f *apiFailure) (journalhttpapi.CreateJournalIllustrationResponseObject, error) {
		return journalhttpapi.CreateJournalIllustrationdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.imagePrincipal(ctx, id, false)
	if f != nil {
		return deny(f)
	}
	if f = s.operationsFailure(ctx, "journal.illustration", id.String()); f != nil {
		return deny(f)
	}
	if r.Body == nil || r.Body.RequestID != id || r.Body.ConsentRevision != privacy.JournalIllustrationDocumentVersion {
		return deny(imageFailure(illustration.ErrInput, id.String()))
	}
	body := r.Body
	confirmation := illustration.Confirmation{RequestID: id.String(), VersionID: body.VersionID.String(), ConsentRevision: body.ConsentRevision, Brief: illustration.Brief{Title: body.Brief.Title}}
	for _, detail := range body.Brief.Details {
		confirmation.Brief.Details = append(confirmation.Brief.Details, illustration.Detail{Kind: string(detail.Kind), Text: detail.Text})
	}
	var reference []byte
	if body.Reference != nil {
		reference = *body.Reference
	}
	if body.ReferenceSHA256 != nil {
		confirmation.ReferenceSHA256 = *body.ReferenceSHA256
	}
	job, err := s.journalIllustrations.Create(ctx, p.KeyID, confirmation, reference)
	if err != nil {
		return deny(imageFailure(err, id.String()))
	}
	return journalhttpapi.CreateJournalIllustration200JSONResponse(imageTaskResponse(job)), nil
}

func (s *Server) GetJournalIllustration(ctx context.Context, r journalhttpapi.GetJournalIllustrationRequestObject) (journalhttpapi.GetJournalIllustrationResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	deny := func(f *apiFailure) (journalhttpapi.GetJournalIllustrationResponseObject, error) {
		return journalhttpapi.GetJournalIllustrationdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.imagePrincipal(ctx, id, false)
	if f != nil {
		return deny(f)
	}
	job, err := s.journalIllustrations.Get(ctx, p.KeyID, r.TaskID.String())
	if err != nil {
		return deny(imageFailure(err, id.String()))
	}
	return journalhttpapi.GetJournalIllustration200JSONResponse(imageTaskResponse(job)), nil
}

func (s *Server) CancelJournalIllustration(ctx context.Context, r journalhttpapi.CancelJournalIllustrationRequestObject) (journalhttpapi.CancelJournalIllustrationResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	deny := func(f *apiFailure) (journalhttpapi.CancelJournalIllustrationResponseObject, error) {
		return journalhttpapi.CancelJournalIllustrationdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.imagePrincipal(ctx, id, true)
	if f != nil {
		return deny(f)
	}
	if r.Body == nil || r.Body.Revision < 1 {
		return deny(imageFailure(illustration.ErrInput, id.String()))
	}
	job, err := s.journalIllustrations.Cancel(ctx, p.KeyID, r.TaskID.String(), uint64(r.Body.Revision))
	if err != nil {
		return deny(imageFailure(err, id.String()))
	}
	return journalhttpapi.CancelJournalIllustration200JSONResponse(imageTaskResponse(job)), nil
}

func (s *Server) GetJournalIllustrationResult(ctx context.Context, r journalhttpapi.GetJournalIllustrationResultRequestObject) (journalhttpapi.GetJournalIllustrationResultResponseObject, error) {
	id := r.Params.XTellyouwhatRequestID
	deny := func(f *apiFailure) (journalhttpapi.GetJournalIllustrationResultResponseObject, error) {
		return journalhttpapi.GetJournalIllustrationResultdefaultJSONResponse{StatusCode: f.status, Body: journalErrorResponse(f)}, nil
	}
	p, f := s.imagePrincipal(ctx, id, false)
	if f != nil {
		return deny(f)
	}
	job, err := s.journalIllustrations.Get(ctx, p.KeyID, r.TaskID.String())
	if err != nil {
		return deny(imageFailure(err, id.String()))
	}
	if job.Task.State != illustration.TaskSucceeded || job.Result == nil {
		return deny(imageFailure(illustration.ErrTaskConflict, id.String()))
	}
	result := job.Result
	strictGinContext(ctx).Header("Cache-Control", "no-store")
	return journalhttpapi.GetJournalIllustrationResult200JSONResponse{Image: result.Image, Mime: journalhttpapi.IllustrationResultMime(result.MIME), Width: result.Width, Height: result.Height}, nil
}
