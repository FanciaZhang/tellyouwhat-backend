package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/capability"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/healthhttpapi"
	"github.com/tellyouwhat/backend/internal/jobs"
	"github.com/tellyouwhat/backend/internal/quota"
)

func (server *Server) CompleteAIRequest(
	ctx context.Context,
	request healthhttpapi.CompleteAIRequestRequestObject,
) (healthhttpapi.CompleteAIRequestResponseObject, error) {
	artifact, principal, managed, lease, failure := server.apiAuthorizeAIRequest(ctx, request.Params.XTellyouwhatRequestID, request.Body)
	if failure != nil {
		return healthhttpapi.CompleteAIRequestdefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	response, err := server.provider.Complete(ctx, artifact)
	if err != nil {
		lease.Release(contracts.ReservationTokens(artifact))
		failure = newAPIFailure(http.StatusBadGateway, "upstream_error", "managed AI provider failed", artifact.RequestID)
		return healthhttpapi.CompleteAIRequest502JSONResponse{BadGatewayJSONResponse: healthhttpapi.BadGatewayJSONResponse(healthErrorResponse(failure))}, nil
	}
	actualTokens := response.InputTokens + response.OutputTokens
	if err := server.recordUsage(ctx, principal, artifact, managed, response.InputTokens, response.OutputTokens); err != nil {
		actualTokens = contracts.ReservationTokens(artifact)
	}
	cleanupManagedMedia(ctx, server.provider, artifact.Media)
	lease.Release(actualTokens)
	return healthhttpapi.CompleteAIRequest200JSONResponse{
		RequestID: request.Params.XTellyouwhatRequestID,
		Content:   response.Content,
		Usage: healthhttpapi.TokenUsage{
			InputTokens: response.InputTokens, OutputTokens: response.OutputTokens,
		},
	}, nil
}

func (server *Server) apiAuthorizeAIRequest(
	ctx context.Context,
	requestID uuid.UUID,
	body *healthhttpapi.AIRequest,
) (contracts.Request, Principal, bool, quota.Releaser, *apiFailure) {
	artifact, rawBody, principal, managed, failure := server.apiValidateAIRequest(ctx, requestID, body)
	if failure != nil {
		return contracts.Request{}, Principal{}, false, nil, failure
	}
	lease, failure := server.apiAcquireQuota(ctx, principal, artifact, "", managed)
	if failure != nil {
		return contracts.Request{}, Principal{}, false, nil, failure
	}
	if err := server.media.Consume(ctx, principal, artifact, contracts.BodySHA256(rawBody)); err != nil {
		lease.Release(0)
		return contracts.Request{}, Principal{}, false, nil, server.apiAdmissionFailure(err, artifact.RequestID)
	}
	return artifact, principal, managed, lease, nil
}

func (server *Server) StreamAIRequest(
	ctx context.Context,
	request healthhttpapi.StreamAIRequestRequestObject,
) (healthhttpapi.StreamAIRequestResponseObject, error) {
	artifact, principal, managed, lease, failure := server.apiAuthorizeAIRequest(ctx, request.Params.XTellyouwhatRequestID, request.Body)
	if failure != nil {
		return healthhttpapi.StreamAIRequestdefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	ginContext := strictGinContext(ctx)
	ginContext.Header("Cache-Control", "no-cache, no-transform")
	ginContext.Header("X-Accel-Buffering", "no")
	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		actualTokens := contracts.ReservationTokens(artifact)
		err := server.provider.Stream(ctx, artifact, func(event StreamEvent) error {
			switch {
			case event.Completed != nil:
				if err := server.recordUsage(ctx, principal, artifact, managed, event.Completed.InputTokens, event.Completed.OutputTokens); err == nil {
					actualTokens = event.Completed.InputTokens + event.Completed.OutputTokens
				}
				return writeSSE(writer, "completed", map[string]any{
					"requestID": artifact.RequestID,
					"content":   event.Completed.Content,
					"usage": map[string]int{
						"inputTokens": event.Completed.InputTokens, "outputTokens": event.Completed.OutputTokens,
					},
				})
			case event.Delta != "":
				return writeSSE(writer, "delta", map[string]string{"delta": event.Delta})
			default:
				return nil
			}
		})
		if err == nil {
			cleanupManagedMedia(ctx, server.provider, artifact.Media)
		} else {
			_ = writeSSE(writer, "error", map[string]string{
				"code": "upstream_error", "requestID": artifact.RequestID,
			})
		}
		lease.Release(actualTokens)
	}()
	return healthhttpapi.StreamAIRequest200TexteventStreamResponse{Body: reader}, nil
}

func (server *Server) IssueAIJobCapability(
	ctx context.Context,
	request healthhttpapi.IssueAIJobCapabilityRequestObject,
) (healthhttpapi.IssueAIJobCapabilityResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.capabilities == nil || server.jobs == nil || server.dispatcher == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", requestID.String())
		return healthhttpapi.IssueAIJobCapability503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	artifact, rawBody, principal, managed, failure := server.apiValidateAIRequest(ctx, requestID, request.Body)
	if failure != nil {
		return healthhttpapi.IssueAIJobCapabilitydefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	bodyDigest := contracts.BodySHA256(rawBody)
	lease, failure := server.apiAcquireQuota(ctx, principal, artifact, capabilityQuotaReservationID(principal, artifact.RequestID, bodyDigest), managed)
	if failure != nil {
		return healthhttpapi.IssueAIJobCapabilitydefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	attempt, _, err := server.media.Admit(ctx, principal, artifact, bodyDigest)
	if err != nil {
		lease.Release(contracts.ReservationTokens(artifact))
		failure = server.apiAdmissionFailure(err, artifact.RequestID)
		return healthhttpapi.IssueAIJobCapabilitydefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	lease.Release(contracts.ReservationTokens(artifact))
	mediaDigest, err := contracts.MediaDigest(artifact.Media)
	if err != nil {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request violates the business contract", artifact.RequestID)
		return healthhttpapi.IssueAIJobCapability422JSONResponse{UnprocessableEntityJSONResponse: healthhttpapi.UnprocessableEntityJSONResponse(healthErrorResponse(failure))}, nil
	}
	issued, err := server.capabilities.IssueAt(principal, capability.Binding{
		RequestID: artifact.RequestID, Operation: artifact.Operation,
		BodyDigest: bodyDigest, MediaDigest: mediaDigest,
	}, attempt.CreatedAt)
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", artifact.RequestID)
		return healthhttpapi.IssueAIJobCapability503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	jobID, err := uuid.Parse(issued.JobID)
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", artifact.RequestID)
		return healthhttpapi.IssueAIJobCapability503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	return healthhttpapi.IssueAIJobCapability201JSONResponse{
		JobID: jobID, Token: issued.Token, ExpiresAt: issued.ExpiresAt,
	}, nil
}

func (server *Server) EnqueueAIJob(
	ctx context.Context,
	request healthhttpapi.EnqueueAIJobRequestObject,
) (healthhttpapi.EnqueueAIJobResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.jobs == nil || server.dispatcher == nil || server.capabilities == nil || server.entitlements == nil || server.contracts == nil || server.media == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", requestID.String())
		return healthhttpapi.EnqueueAIJob503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	artifact, failure := apiRequest(request.Body, requestID.String())
	if failure != nil {
		return healthhttpapi.EnqueueAIJobdefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if err := server.contracts.Validate(artifact); err != nil {
		failure = mappedContractFailure(err, artifact.RequestID)
		return healthhttpapi.EnqueueAIJobdefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	rawBody := rawRequestBody(strictGinContext(ctx))
	mediaDigest, err := contracts.MediaDigest(artifact.Media)
	if err != nil {
		failure = newAPIFailure(http.StatusUnprocessableEntity, "contract_violation", "request violates the business contract", artifact.RequestID)
		return healthhttpapi.EnqueueAIJob422JSONResponse{UnprocessableEntityJSONResponse: healthhttpapi.UnprocessableEntityJSONResponse(healthErrorResponse(failure))}, nil
	}
	binding := capability.Binding{
		JobID: request.Params.XHealthJobID.String(), RequestID: artifact.RequestID,
		Operation: artifact.Operation, BodyDigest: contracts.BodySHA256(rawBody), MediaDigest: mediaDigest,
	}
	principal, err := server.capabilities.Validate(request.Params.XHealthJobCapability, binding)
	if err != nil {
		failure = newAPIFailure(http.StatusUnauthorized, "job_capability_invalid", "job capability is invalid or expired", artifact.RequestID)
		return healthhttpapi.EnqueueAIJob401JSONResponse{UnauthorizedJSONResponse: healthhttpapi.UnauthorizedJSONResponse(healthErrorResponse(failure))}, nil
	}
	managed, failure := server.apiAuthorizeAIEntitlement(ctx, principal, artifact, artifact.RequestID)
	if failure != nil {
		return healthhttpapi.EnqueueAIJobdefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	job, err := server.jobs.EnqueueWithID(ctx, principalForQuota(principal, managed), binding.JobID, artifact, binding.BodyDigest)
	if err != nil {
		if errors.Is(err, jobs.ErrIdempotencyConflict) {
			failure = newAPIFailure(http.StatusConflict, "idempotency_conflict", "requestID was already used with different content", artifact.RequestID)
			return healthhttpapi.EnqueueAIJob409JSONResponse{ConflictJSONResponse: healthhttpapi.ConflictJSONResponse(healthErrorResponse(failure))}, nil
		}
		failure = newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", artifact.RequestID)
		return healthhttpapi.EnqueueAIJob503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	_, _ = server.capabilities.Consume(ctx, request.Params.XHealthJobCapability, binding)
	_ = server.dispatcher.Dispatch(ctx, job.ID)
	value, err := apiJob(job)
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", artifact.RequestID)
		return healthhttpapi.EnqueueAIJob503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	return healthhttpapi.EnqueueAIJob202JSONResponse(value), nil
}

func (server *Server) GetAIJob(
	ctx context.Context,
	request healthhttpapi.GetAIJobRequestObject,
) (healthhttpapi.GetAIJobResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.jobs == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", requestID.String())
		return healthhttpapi.GetAIJob503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return healthhttpapi.GetAIJobdefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	job, err := server.jobs.Get(ctx, principal, request.Id.String())
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			failure = newAPIFailure(http.StatusNotFound, "job_not_found", "job not found", requestID.String())
			return healthhttpapi.GetAIJob404JSONResponse{NotFoundJSONResponse: healthhttpapi.NotFoundJSONResponse(healthErrorResponse(failure))}, nil
		}
		failure = newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", requestID.String())
		return healthhttpapi.GetAIJob503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	value, err := apiJob(job)
	if err != nil {
		failure = newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", requestID.String())
		return healthhttpapi.GetAIJob503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	return healthhttpapi.GetAIJob200JSONResponse(value), nil
}

func (server *Server) CancelAIJob(
	ctx context.Context,
	request healthhttpapi.CancelAIJobRequestObject,
) (healthhttpapi.CancelAIJobResponseObject, error) {
	requestID := request.Params.XTellyouwhatRequestID
	if server.jobs == nil {
		failure := newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", requestID.String())
		return healthhttpapi.CancelAIJob503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
	}
	principal, failure := server.apiAuthenticate(ctx, requestID)
	if failure != nil {
		return healthhttpapi.CancelAIJobdefaultJSONResponse{Body: healthErrorResponse(failure), StatusCode: failure.status}, nil
	}
	if err := server.jobs.Cancel(ctx, principal, request.Id.String()); err != nil {
		switch {
		case errors.Is(err, jobs.ErrNotFound):
			failure = newAPIFailure(http.StatusNotFound, "job_not_found", "job not found", requestID.String())
			return healthhttpapi.CancelAIJob404JSONResponse{NotFoundJSONResponse: healthhttpapi.NotFoundJSONResponse(healthErrorResponse(failure))}, nil
		case errors.Is(err, jobs.ErrJobNotClaimable):
			failure = newAPIFailure(http.StatusConflict, "job_not_cancellable", "job is already complete", requestID.String())
			return healthhttpapi.CancelAIJob409JSONResponse{ConflictJSONResponse: healthhttpapi.ConflictJSONResponse(healthErrorResponse(failure))}, nil
		default:
			failure = newAPIFailure(http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", requestID.String())
			return healthhttpapi.CancelAIJob503JSONResponse{ServiceUnavailableJSONResponse: healthhttpapi.ServiceUnavailableJSONResponse(healthErrorResponse(failure))}, nil
		}
	}
	return healthhttpapi.CancelAIJob204Response{}, nil
}
