package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/healthhttpapi"
	journalcontracts "github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"github.com/tellyouwhat/backend/internal/observability"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/platformhttpapi"
)

const rawRequestBodyKey = "gateway.raw-request-body"

var configureGinOnce sync.Once
var openAPIPathParameter = regexp.MustCompile(`\{([^}/]+)\}`)

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
	router.Use(operationIDMiddleware(server.operationIDs()))
	router.Use(captureRawRequestBody())

	server.registerPlatformRoutes(router)
	switch server.app.ID {
	case appregistry.Health:
		server.registerHealthRoutes(router)
	case appregistry.Journal:
		server.registerJournalRoutes(router)
	default:
		panic("unsupported App router")
	}
	return router
}

func (server *Server) registerPlatformRoutes(router gin.IRouter) {
	strict := platformhttpapi.NewStrictHandlerWithOptions(server, nil, platformhttpapi.StrictGinServerOptions{
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
		HandlerErrorFunc:         strictHandlerError,
		ResponseErrorHandlerFunc: strictResponseError,
	})
	platformhttpapi.RegisterHandlersWithOptions(router, strict, platformhttpapi.GinServerOptions{ErrorHandler: parameterError})
}

func (server *Server) registerHealthRoutes(router gin.IRouter) {
	strict := healthhttpapi.NewStrictHandlerWithOptions(server, nil, healthhttpapi.StrictGinServerOptions{
		RequestErrorHandlerFunc: func(context *gin.Context, _ error) {
			writeTransportError(context, http.StatusUnprocessableEntity, "contract_violation", "request violates the transport contract")
		},
		HandlerErrorFunc:         strictHandlerError,
		ResponseErrorHandlerFunc: strictResponseError,
	})
	healthhttpapi.RegisterHandlersWithOptions(router, strict, healthhttpapi.GinServerOptions{ErrorHandler: parameterError})
}

func (server *Server) registerJournalRoutes(router gin.IRouter) {
	strict := journalhttpapi.NewStrictHandlerWithOptions(server, nil, journalhttpapi.StrictGinServerOptions{
		RequestErrorHandlerFunc: func(context *gin.Context, _ error) {
			writeTransportError(context, http.StatusUnprocessableEntity, "contract_violation", "request violates the transport contract")
		},
		HandlerErrorFunc:         strictHandlerError,
		ResponseErrorHandlerFunc: strictResponseError,
	})
	journalhttpapi.RegisterHandlersWithOptions(router, strict, journalhttpapi.GinServerOptions{ErrorHandler: parameterError})
}

func strictHandlerError(context *gin.Context, _ error) {
	writeTransportError(context, http.StatusInternalServerError, "internal_error", "request could not be completed")
}

func strictResponseError(context *gin.Context, _ error) {
	if !context.Writer.Written() {
		writeTransportError(context, http.StatusInternalServerError, "response_encoding_failed", "response could not be encoded")
	}
}

func parameterError(context *gin.Context, _ error, status int) {
	writeTransportError(context, status, "invalid_parameter", "request parameter is invalid")
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
	case "POST /v1/ai/operations/journal.organize/responses":
		return journalcontracts.MaxBodyBytes
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
		return nil
	}
	body, _ := value.([]byte)
	return body
}

func writeTransportError(context *gin.Context, status int, code, message string) {
	requestID := strings.ToLower(context.GetHeader("X-Tellyouwhat-Request-ID"))
	context.JSON(status, platformhttpapi.ErrorResponse{Error: platformhttpapi.ErrorDetail{
		Code: code, Message: message, RequestID: requestID,
	}})
}

func (server *Server) operationIDs() map[string]string {
	documents := []*openapi3.T{mustOpenAPI(platformhttpapi.GetSwagger())}
	switch server.app.ID {
	case appregistry.Health:
		documents = append(documents, mustOpenAPI(healthhttpapi.GetSwagger()))
	case appregistry.Journal:
		documents = append(documents, mustOpenAPI(journalhttpapi.GetSwagger()))
	}
	result := make(map[string]string)
	for _, document := range documents {
		for path, item := range document.Paths.Map() {
			ginPath := openAPIPathParameter.ReplaceAllString(path, `:$1`)
			for method, operation := range item.Operations() {
				result[strings.ToUpper(method)+" "+ginPath] = canonicalOperationID(operation.OperationID)
			}
		}
	}
	return result
}

func canonicalOperationID(value string) string {
	if value == "" || value[0] < 'A' || value[0] > 'Z' {
		return value
	}
	return string(value[0]+('a'-'A')) + value[1:]
}

func mustOpenAPI(document *openapi3.T, err error) *openapi3.T {
	if err != nil {
		panic("generated OpenAPI contract is invalid: " + err.Error())
	}
	return document
}

func operationIDMiddleware(operationIDs map[string]string) gin.HandlerFunc {
	return func(context *gin.Context) {
		if operationID := operationIDs[context.Request.Method+" "+context.FullPath()]; operationID != "" {
			context.Set(observability.OperationIDContextKey, operationID)
		}
		context.Next()
	}
}
