package observability

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
)

const OperationIDContextKey = "tellyouwhat.operation_id"

// Recovery is Gin-native panic recovery for every HTTP runtime.
func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return recovery(logger, "")
}

func recovery(logger *slog.Logger, appID string) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	if appID != "" {
		logger = logger.With("app_id", appID)
	}
	return func(context *gin.Context) {
		defer func() {
			if value := recover(); value != nil {
				logger.ErrorContext(context.Request.Context(), "http handler panic",
					"panic_type", panicType(value),
					"stack", string(debug.Stack()),
					"request_id", requestID(context.Request),
					"host", context.Request.Host,
					"operation_id", operationID(context),
				)
				if !context.Writer.Written() {
					context.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
						"error": gin.H{"code": "internal_error", "message": "internal server error"},
					})
				} else {
					context.Abort()
				}
			}
		}()
		context.Next()
	}
}

func panicType(value any) string {
	switch value.(type) {
	case error:
		return "error"
	case string:
		return "string"
	default:
		return "unknown"
	}
}

// Logger records a request after Gin has completed its handler chain.
func Logger(logger *slog.Logger) gin.HandlerFunc {
	return loggerMiddleware(logger, "")
}

func loggerMiddleware(logger *slog.Logger, appID string) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	if appID != "" {
		logger = logger.With("app_id", appID)
	}
	return func(context *gin.Context) {
		startedAt := time.Now()
		context.Next()
		request := context.Request
		logger.InfoContext(request.Context(), "http request",
			"method", request.Method,
			"path", request.URL.EscapedPath(),
			"status", context.Writer.Status(),
			"duration_ms", time.Since(startedAt).Milliseconds(),
			"request_id", requestID(request),
			"host", request.Host,
			"operation_id", operationID(context),
		)
	}
}

// Middleware returns the common Gin middleware in request order.
func Middleware(logger *slog.Logger) []gin.HandlerFunc {
	return []gin.HandlerFunc{Logger(logger), Recovery(logger)}
}

// MiddlewareForApp adds stable App identity to every public gateway log entry.
func MiddlewareForApp(logger *slog.Logger, appID string) []gin.HandlerFunc {
	return []gin.HandlerFunc{loggerMiddleware(logger, appID), recovery(logger, appID)}
}

func operationID(context *gin.Context) string {
	if value := context.GetString(OperationIDContextKey); value != "" {
		return value
	}
	return "unmatched"
}

func requestID(request *http.Request) string {
	if value := request.Header.Get("X-Tellyouwhat-Request-ID"); value != "" {
		return value
	}
	return request.Header.Get("X-Request-ID")
}
