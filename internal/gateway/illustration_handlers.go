package gateway

import (
	"context"
	"database/sql"
	"errors"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"math"
	"net/http"
)

func (s *Server) illustrationAdmission(ctx context.Context, id uuid.UUID) (Principal, *apiFailure) {
	strictGinContext(ctx).Header("Cache-Control", "no-store")
	principal, failure := s.apiAuthenticate(ctx, id)
	if failure != nil {
		return Principal{}, failure
	}
	if s.journalIllustrations == nil {
		return Principal{}, newAPIFailure(503, "not_ready", "image service unavailable", id.String())
	}
	return principal, nil
}
func illustrationFailure(err error, id uuid.UUID) *apiFailure {
	status, code, message := 503, "image_unavailable", "image service unavailable"
	switch {
	case errors.Is(err, illustration.ErrImageBudget):
		status, code, message = 429, "image_quota_unavailable", "image allowance unavailable or exhausted"
	case errors.Is(err, illustration.ErrAccessDenied):
		status, code, message = 403, "image_access_denied", "image authorization is unavailable or revoked"
	case errors.Is(err, illustration.ErrInput):
		status, code, message = 422, "contract_violation", "invalid image request"
	case errors.Is(err, illustration.ErrTaskConflict):
		status, code, message = 409, "image_task_changed", "image task changed or is not ready"
	case errors.Is(err, sql.ErrNoRows):
		status, code, message = 404, "not_found", "image task not found"
	}
	return newAPIFailure(status, code, message, id.String())
}
func illustrationTaskResponse(task illustration.Task) (journalhttpapi.IllustrationTask, error) {
	id, e1 := uuid.Parse(task.ID)
	version, e2 := uuid.Parse(task.VersionID)
	if e1 != nil || e2 != nil || id == uuid.Nil || version == uuid.Nil || task.Revision == 0 || task.Revision > math.MaxInt64 || task.ExpiresAt.IsZero() {
		return journalhttpapi.IllustrationTask{}, illustration.ErrResult
	}
	switch task.State {
	case illustration.TaskQueued, illustration.TaskRunning, illustration.TaskSucceeded, illustration.TaskRejected, illustration.TaskUncertain, illustration.TaskCancelled:
	default:
		return journalhttpapi.IllustrationTask{}, illustration.ErrResult
	}
	return journalhttpapi.IllustrationTask{Id: id, VersionID: version, State: journalhttpapi.IllustrationTaskState(task.State), Revision: int64(task.Revision), ExpiresAt: task.ExpiresAt}, nil
}

func (s *Server) CreateJournalIllustration(ctx context.Context, request journalhttpapi.CreateJournalIllustrationRequestObject) (journalhttpapi.CreateJournalIllustrationResponseObject, error) {
	id := request.Params.XTellyouwhatRequestID
	principal, failure := s.illustrationAdmission(ctx, id)
	var task illustration.Task
	var err error
	if failure == nil {
		if request.Body == nil {
			err = illustration.ErrInput
		} else if request.Body.RequestID != id {
			failure = newAPIFailure(http.StatusUnauthorized, "authentication_failed", "request authentication failed", id.String())
		} else {
			b := request.Body
			c := illustration.Confirmation{RequestID: b.RequestID.String(), VersionID: b.VersionID.String(), Prompt: b.Prompt, ConsentRevision: b.ConsentRevision}
			if b.ReferenceSHA256 != nil {
				c.ReferenceSHA256 = *b.ReferenceSHA256
			}
			var reference []byte
			if b.Reference != nil {
				reference = *b.Reference
			}
			task, err = s.journalIllustrations.Create(ctx, principal, c, reference)
		}
	}
	if failure == nil && err != nil {
		failure = illustrationFailure(err, id)
	}
	var output journalhttpapi.IllustrationTask
	if failure == nil {
		output, err = illustrationTaskResponse(task)
		if err != nil {
			failure = illustrationFailure(err, id)
		}
	}
	if failure != nil {
		return journalhttpapi.CreateJournalIllustrationdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.CreateJournalIllustration200JSONResponse(output), nil
}

func (s *Server) GetJournalIllustration(ctx context.Context, request journalhttpapi.GetJournalIllustrationRequestObject) (journalhttpapi.GetJournalIllustrationResponseObject, error) {
	id := request.Params.XTellyouwhatRequestID
	principal, failure := s.illustrationAdmission(ctx, id)
	var task illustration.Task
	var err error
	if failure == nil {
		task, err = s.journalIllustrations.Status(ctx, principal, request.TaskID.String())
	}
	if failure == nil && err != nil {
		failure = illustrationFailure(err, id)
	}
	var output journalhttpapi.IllustrationTask
	if failure == nil {
		output, err = illustrationTaskResponse(task)
		if err != nil {
			failure = illustrationFailure(err, id)
		}
	}
	if failure != nil {
		return journalhttpapi.GetJournalIllustrationdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.GetJournalIllustration200JSONResponse(output), nil
}

func (s *Server) CancelJournalIllustration(ctx context.Context, request journalhttpapi.CancelJournalIllustrationRequestObject) (journalhttpapi.CancelJournalIllustrationResponseObject, error) {
	id := request.Params.XTellyouwhatRequestID
	principal, failure := s.illustrationAdmission(ctx, id)
	var task illustration.Task
	var err error
	if failure == nil {
		if request.Body == nil || request.Body.Revision < 1 {
			err = illustration.ErrInput
		} else {
			task, err = s.journalIllustrations.Cancel(ctx, principal, request.TaskID.String(), uint64(request.Body.Revision))
		}
	}
	if failure == nil && err != nil {
		failure = illustrationFailure(err, id)
	}
	var output journalhttpapi.IllustrationTask
	if failure == nil {
		output, err = illustrationTaskResponse(task)
		if err != nil {
			failure = illustrationFailure(err, id)
		}
	}
	if failure != nil {
		return journalhttpapi.CancelJournalIllustrationdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.CancelJournalIllustration200JSONResponse(output), nil
}

func (s *Server) GetJournalIllustrationResult(ctx context.Context, request journalhttpapi.GetJournalIllustrationResultRequestObject) (journalhttpapi.GetJournalIllustrationResultResponseObject, error) {
	id := request.Params.XTellyouwhatRequestID
	principal, failure := s.illustrationAdmission(ctx, id)
	var result illustration.Result
	if failure == nil {
		var err error
		result, err = s.journalIllustrations.Result(ctx, principal, request.TaskID.String())
		if err != nil {
			failure = illustrationFailure(err, id)
		}
	}
	if failure != nil {
		return journalhttpapi.GetJournalIllustrationResultdefaultJSONResponse{Body: journalErrorResponse(failure), StatusCode: failure.status}, nil
	}
	return journalhttpapi.GetJournalIllustrationResult200JSONResponse{Image: result.Image, Mime: journalhttpapi.IllustrationResultMime(result.MIME), Width: result.Width, Height: result.Height}, nil
}
