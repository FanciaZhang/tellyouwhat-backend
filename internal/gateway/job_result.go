package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tellyouwhat/backend/internal/healthhttpapi"
	"github.com/tellyouwhat/backend/internal/jobs"
)

func (server *Server) DownloadAIJobResult(ctx context.Context, request healthhttpapi.DownloadAIJobResultRequestObject) (healthhttpapi.DownloadAIJobResultResponseObject, error) {
	strictGinContext(ctx).Header("Cache-Control", "no-store")
	strictGinContext(ctx).Header("X-Accel-Buffering", "no")
	return server.waitForJobResult(ctx, request, 4*time.Minute, time.Second)
}

func (server *Server) waitForJobResult(ctx context.Context, request healthhttpapi.DownloadAIJobResultRequestObject, maximumWait, interval time.Duration) (healthhttpapi.DownloadAIJobResultResponseObject, error) {
	failure := func(status int, code string) healthhttpapi.DownloadAIJobResultResponseObject {
		value := newAPIFailure(status, code, "job result unavailable", request.Params.XTellyouwhatRequestID.String())
		return healthhttpapi.DownloadAIJobResultdefaultJSONResponse{Body: healthErrorResponse(value), StatusCode: status}
	}
	if server.jobs == nil || server.capabilities == nil {
		return failure(http.StatusServiceUnavailable, "jobs_unavailable"), nil
	}
	deadline := time.NewTimer(maximumWait)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timedOut := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Recheck expiry and fetch the durable row on every iteration. Privacy
		// deletion and cancellation cannot be bypassed by a retained result.
		principal, requestID, err := server.capabilities.ValidateResult(request.Params.XHealthJobResultCapability, request.Id.String())
		if err != nil || principal.AppID != string(server.app.ID) {
			return failure(http.StatusUnauthorized, "job_result_capability_invalid"), nil
		}
		job, err := server.jobs.Get(ctx, principal, request.Id.String())
		if errors.Is(err, jobs.ErrNotFound) {
			return failure(http.StatusNotFound, "job_not_found"), nil
		}
		if err != nil {
			return failure(http.StatusServiceUnavailable, "jobs_unavailable"), nil
		}
		if job.RequestID != requestID || job.AppID != principal.AppID || job.OwnerDeviceID != principal.DeviceID {
			return failure(http.StatusNotFound, "job_not_found"), nil
		}
		if timedOut || (job.Status != jobs.StatusQueued && job.Status != jobs.StatusRunning) {
			value, err := apiJob(job)
			if err != nil {
				return failure(http.StatusServiceUnavailable, "jobs_unavailable"), nil
			}
			return healthhttpapi.DownloadAIJobResult200JSONResponse(value), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			timedOut = true
		case <-ticker.C:
		}
	}
}
