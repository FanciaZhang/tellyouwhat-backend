package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/tellyouwhat/backend/internal/contracts"
	journalcontracts "github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
)

const journalRawRequestBodyKey = "journal.raw-request-body"

func (server *Server) newJournalHTTPRouter() http.Handler {
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
	router.Use(captureJournalRawRequestBody())

	strictServer := &journalStrictServer{server: server}
	strict := journalhttpapi.NewStrictHandlerWithOptions(strictServer, nil, journalhttpapi.StrictGinServerOptions{
		RequestErrorHandlerFunc: func(context *gin.Context, _ error) {
			status := http.StatusUnprocessableEntity
			code := "contract_violation"
			message := "request violates the transport contract"
			if context.FullPath() == "/v1/app-store/notifications" {
				status = http.StatusBadRequest
				code = "invalid_notification_envelope"
				message = "notification envelope is invalid"
			}
			writeJournalTransportError(context, status, code, message)
		},
		HandlerErrorFunc: func(context *gin.Context, _ error) {
			writeJournalTransportError(context, http.StatusInternalServerError, "internal_error", "request could not be completed")
		},
		ResponseErrorHandlerFunc: func(context *gin.Context, _ error) {
			if !context.Writer.Written() {
				writeJournalTransportError(context, http.StatusInternalServerError, "response_encoding_failed", "response could not be encoded")
			}
		},
	})
	journalhttpapi.RegisterHandlersWithOptions(router, strict, journalhttpapi.GinServerOptions{
		ErrorHandler: func(context *gin.Context, _ error, status int) {
			writeJournalTransportError(context, status, "invalid_parameter", "request parameter is invalid")
		},
	})
	return router
}

func captureJournalRawRequestBody() gin.HandlerFunc {
	return func(context *gin.Context) {
		limit := journalRequestBodyLimit(context.Request.Method, context.Request.URL.Path)
		body, err := readLimitedBody(context.Request.Body, limit)
		if err != nil {
			if limit == 0 && errors.Is(err, contracts.ErrPayloadTooLarge) {
				writeJournalTransportError(context, http.StatusUnprocessableEntity, "unexpected_body", "this operation does not accept a request body")
			} else if errors.Is(err, contracts.ErrPayloadTooLarge) {
				writeJournalTransportError(context, http.StatusRequestEntityTooLarge, "payload_too_large", "request exceeds the operation limit")
			} else {
				writeJournalTransportError(context, http.StatusUnprocessableEntity, "contract_violation", "request body could not be read")
			}
			context.Abort()
			return
		}
		if limit == 0 && len(body) != 0 {
			writeJournalTransportError(context, http.StatusUnprocessableEntity, "unexpected_body", "this operation does not accept a request body")
			context.Abort()
			return
		}
		context.Set(journalRawRequestBodyKey, body)
		context.Request.Body = io.NopCloser(bytes.NewReader(body))
		context.Next()
	}
}

func journalRequestBodyLimit(method, path string) int64 {
	switch method + " " + path {
	case "POST /v1/attest/challenges":
		return 8 << 10
	case "POST /v1/attest/keys":
		return 512 << 10
	case "POST /v1/entitlements/transactions", "POST /v1/app-store/notifications":
		return 1 << 20
	case "POST /v1/privacy/consents":
		return 16 << 10
	case "POST /v1/ai/operations/journal.organize/responses":
		return journalcontracts.MaxBodyBytes
	default:
		return 0
	}
}

func journalRawRequestBody(context *gin.Context) []byte {
	value, exists := context.Get(journalRawRequestBodyKey)
	if !exists {
		return nil
	}
	body, _ := value.([]byte)
	return body
}

func writeJournalTransportError(context *gin.Context, status int, code, message string) {
	requestID := strings.ToLower(context.GetHeader("X-Tellyouwhat-Request-ID"))
	context.JSON(status, journalhttpapi.ErrorResponse{Error: journalhttpapi.ErrorDetail{
		Code: code, Message: message, RequestID: requestID,
	}})
}
