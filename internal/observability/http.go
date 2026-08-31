package observability

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func RecoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				logger.ErrorContext(request.Context(), "http handler panic",
					"panic_type", panicType(value),
					"stack", string(debug.Stack()),
					"request_id", request.Header.Get("X-Tellyouwhat-Request-ID"),
				)
				http.Error(writer, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(writer, request)
	})
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

func (writer *statusWriter) WriteHeader(status int) {
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *statusWriter) Flush() {
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func HTTPLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		startedAt := time.Now()
		captured := &statusWriter{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(captured, request)
		logger.InfoContext(request.Context(), "http request",
			"method", request.Method,
			"path", request.URL.EscapedPath(),
			"status", captured.status,
			"duration_ms", time.Since(startedAt).Milliseconds(),
			"request_id", request.Header.Get("X-Tellyouwhat-Request-ID"),
		)
	})
}
