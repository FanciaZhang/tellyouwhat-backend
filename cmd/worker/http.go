package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/tellyouwhat/backend/internal/jobs"
	"github.com/tellyouwhat/backend/internal/observability"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/workerhttpapi"
)

const workerRequestBodyLimit int64 = 8 << 10

type workerHTTPServer struct {
	secret  string
	workers map[appregistry.AppID]*jobs.Worker
}

var _ workerhttpapi.StrictServerInterface = (*workerHTTPServer)(nil)

var configureWorkerGinOnce sync.Once

func newWorkerRouter(secret string, workers map[appregistry.AppID]*jobs.Worker, logger *slog.Logger) *gin.Engine {
	configureWorkerGinOnce.Do(func() {
		gin.SetMode(gin.ReleaseMode)
		binding.EnableDecoderDisallowUnknownFields = true
		binding.EnableDecoderUseNumber = true
	})

	router := gin.New()
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.HandleMethodNotAllowed = true
	router.UseRawPath = true
	router.UnescapePathValues = false
	router.Use(observability.Middleware(logger)...)
	router.Use(authenticateWorker(secret))
	router.Use(limitWorkerRequestBody())

	implementation := &workerHTTPServer{secret: secret, workers: workers}
	strict := workerhttpapi.NewStrictHandlerWithOptions(implementation, nil, workerhttpapi.StrictGinServerOptions{
		RequestErrorHandlerFunc: func(context *gin.Context, err error) {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeWorkerTransportError(context, http.StatusRequestEntityTooLarge, "payload_too_large", "request exceeds the worker limit")
				return
			}
			writeWorkerTransportError(context, http.StatusUnprocessableEntity, "invalid_job", "job reference is invalid")
		},
		HandlerErrorFunc: func(context *gin.Context, _ error) {
			writeWorkerTransportError(context, http.StatusInternalServerError, "internal_error", "request could not be completed")
		},
		ResponseErrorHandlerFunc: func(context *gin.Context, _ error) {
			if !context.Writer.Written() {
				writeWorkerTransportError(context, http.StatusInternalServerError, "response_encoding_failed", "response could not be encoded")
			}
		},
	})
	workerhttpapi.RegisterHandlersWithOptions(router, strict, workerhttpapi.GinServerOptions{
		ErrorHandler: func(context *gin.Context, _ error, status int) {
			writeWorkerTransportError(context, status, "invalid_parameter", "request parameter is invalid")
		},
	})
	return router
}

func authenticateWorker(secret string) gin.HandlerFunc {
	expected := []byte(secret)
	return func(context *gin.Context) {
		if context.Request.Method != http.MethodPost || context.Request.URL.Path != "/internal/jobs/process" {
			context.Next()
			return
		}
		provided := []byte(context.GetHeader("X-Tellyouwhat-Worker-Secret"))
		if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
			writeWorkerTransportError(context, http.StatusUnauthorized, "unauthorized", "worker authentication failed")
			context.Abort()
			return
		}
		context.Next()
	}
}

func limitWorkerRequestBody() gin.HandlerFunc {
	return func(context *gin.Context) {
		if context.Request.Method == http.MethodPost && context.Request.URL.Path == "/internal/jobs/process" {
			context.Request.Body = http.MaxBytesReader(context.Writer, context.Request.Body, workerRequestBodyLimit)
		}
		context.Next()
	}
}

func (server *workerHTTPServer) GetWorkerHealth(
	context.Context,
	workerhttpapi.GetWorkerHealthRequestObject,
) (workerhttpapi.GetWorkerHealthResponseObject, error) {
	return workerhttpapi.GetWorkerHealth200JSONResponse{Status: workerhttpapi.Ok}, nil
}

func (server *workerHTTPServer) ProcessJob(
	ctx context.Context,
	request workerhttpapi.ProcessJobRequestObject,
) (workerhttpapi.ProcessJobResponseObject, error) {
	provided := []byte{}
	if request.Params.XTellyouwhatWorkerSecret != nil {
		provided = []byte(*request.Params.XTellyouwhatWorkerSecret)
	}
	expected := []byte(server.secret)
	if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
		failure := workerFailure("unauthorized", "worker authentication failed")
		return workerhttpapi.ProcessJob401JSONResponse{
			UnauthorizedJSONResponse: workerhttpapi.UnauthorizedJSONResponse(failure),
		}, nil
	}
	if request.Body == nil || !request.Body.AppID.Valid() || request.Body.JobID == "" || len(request.Body.JobID) > 128 {
		failure := workerFailure("invalid_job", "job reference is invalid")
		return workerhttpapi.ProcessJob422JSONResponse{
			UnprocessableEntityJSONResponse: workerhttpapi.UnprocessableEntityJSONResponse(failure),
		}, nil
	}
	worker := server.workers[appregistry.AppID(request.Body.AppID)]
	if worker == nil {
		failure := workerFailure("invalid_job", "job reference is invalid")
		return workerhttpapi.ProcessJob422JSONResponse{
			UnprocessableEntityJSONResponse: workerhttpapi.UnprocessableEntityJSONResponse(failure),
		}, nil
	}
	if err := worker.Process(ctx, request.Body.JobID); err != nil {
		failure := workerFailure("job_failed", "job could not be processed")
		return workerhttpapi.ProcessJob502JSONResponse{
			BadGatewayJSONResponse: workerhttpapi.BadGatewayJSONResponse(failure),
		}, nil
	}
	return workerhttpapi.ProcessJob204Response{}, nil
}

func workerFailure(code, message string) workerhttpapi.ErrorResponse {
	return workerhttpapi.ErrorResponse{Error: workerhttpapi.ErrorDetail{Code: code, Message: message}}
}

func writeWorkerTransportError(context *gin.Context, status int, code, message string) {
	context.JSON(status, workerFailure(code, message))
}
