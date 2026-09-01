package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/httpapi"
)

const rawRequestBodyKey = "health.raw-request-body"

var _ httpapi.StrictServerInterface = (*Server)(nil)
var configureGinOnce sync.Once

func (server *Server) newHTTPRouter() *gin.Engine {
	configureGinOnce.Do(func() {
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
	router.Use(server.httpMiddleware...)
	router.Use(captureRawRequestBody())

	strict := httpapi.NewStrictHandlerWithOptions(server, nil, httpapi.StrictGinServerOptions{
		RequestErrorHandlerFunc: func(context *gin.Context, _ error) {
			status := http.StatusUnprocessableEntity
			code := "contract_violation"
			message := "request violates the transport contract"
			if context.FullPath() == "/v1/app-store/notifications" {
				status = http.StatusBadRequest
				code = "invalid_notification_envelope"
				message = "notification envelope is invalid"
			}
			writeTransportError(context, status, code, message)
		},
		HandlerErrorFunc: func(context *gin.Context, _ error) {
			writeTransportError(context, http.StatusInternalServerError, "internal_error", "request could not be completed")
		},
		ResponseErrorHandlerFunc: func(context *gin.Context, _ error) {
			if !context.Writer.Written() {
				writeTransportError(context, http.StatusInternalServerError, "response_encoding_failed", "response could not be encoded")
			}
		},
	})
	httpapi.RegisterHandlersWithOptions(router, strict, httpapi.GinServerOptions{
		ErrorHandler: func(context *gin.Context, _ error, status int) {
			writeTransportError(context, status, "invalid_parameter", "request parameter is invalid")
		},
	})
	return router
}

func captureRawRequestBody() gin.HandlerFunc {
	return func(context *gin.Context) {
		limit := requestBodyLimit(context.Request.Method, context.Request.URL.Path)
		body, err := readLimitedBody(context.Request.Body, limit)
		if err != nil {
			if limit == 0 && errors.Is(err, contracts.ErrPayloadTooLarge) {
				writeTransportError(context, http.StatusUnprocessableEntity, "unexpected_body", "this operation does not accept a request body")
			} else if errors.Is(err, contracts.ErrPayloadTooLarge) {
				writeTransportError(context, http.StatusRequestEntityTooLarge, "payload_too_large", "request exceeds the operation limit")
			} else {
				writeTransportError(context, http.StatusUnprocessableEntity, "contract_violation", "request body could not be read")
			}
			context.Abort()
			return
		}
		if limit == 0 && len(body) != 0 {
			writeTransportError(context, http.StatusUnprocessableEntity, "unexpected_body", "this operation does not accept a request body")
			context.Abort()
			return
		}
		context.Set(rawRequestBodyKey, body)
		context.Request.Body = io.NopCloser(bytes.NewReader(body))
		context.Next()
	}
}

func requestBodyLimit(method, path string) int64 {
	switch method + " " + path {
	case "POST /v1/attest/challenges":
		return 8 << 10
	case "POST /v1/attest/keys":
		return 512 << 10
	case "POST /v1/entitlements/transactions", "POST /v1/app-store/notifications":
		return 1 << 20
	case "POST /v1/privacy/consents":
		return 16 << 10
	case "POST /v1/media/upload-authorizations":
		return 64 << 10
	case "POST /v1/ai/requests", "POST /v1/ai/streams", "POST /v1/ai/job-capabilities", "POST /v1/ai/jobs":
		return contracts.DefaultBodyLimit
	default:
		return 0
	}
}

func readLimitedBody(reader io.ReadCloser, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	defer reader.Close()
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, contracts.ErrPayloadTooLarge
	}
	return body, nil
}

func rawRequestBody(context *gin.Context) []byte {
	value, exists := context.Get(rawRequestBodyKey)
	if !exists {
		value, exists = context.Get(journalRawRequestBodyKey)
		if !exists {
			return nil
		}
	}
	body, _ := value.([]byte)
	return body
}

func writeTransportError(context *gin.Context, status int, code, message string) {
	requestID := strings.ToLower(context.GetHeader("X-Health-Request-ID"))
	context.JSON(status, httpapi.ErrorResponse{Error: httpapi.ErrorDetail{
		Code: code, Message: message, RequestID: requestID,
	}})
}
