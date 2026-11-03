package observability

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
)

func TestRecoverPanicsReturns500(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := gin.New()
	router.Use(Recovery(logger))
	router.GET("/panic", func(*gin.Context) {
		panic("boom")
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", response.Code)
	}
}

func TestStorageFailureLogsOnlyErrorCodeAndRaisesSeverity(t *testing.T) {
	var output bytes.Buffer
	router := gin.New()
	router.Use(MiddlewareForApp(slog.New(slog.NewJSONHandler(&output, nil)), "health")...)
	router.POST("/v1/privacy/consents", func(ctx *gin.Context) {
		RecordStorageFailure(ctx, fmt.Errorf("wrapped: %w", &mysql.MySQLError{Number: 3819, Message: "private SQL health contents"}))
		ctx.Status(http.StatusServiceUnavailable)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/privacy/consents", nil)
	request.Header.Set("X-Request-ID", "fixture-request")
	router.ServeHTTP(httptest.NewRecorder(), request)
	for _, required := range []string{`"level":"ERROR"`, `"sql_error_code":3819`, `"failure_class":"storage"`, `"request_id":"fixture-request"`} {
		if !bytes.Contains(output.Bytes(), []byte(required)) {
			t.Fatalf("missing diagnostic %s: %s", required, output.String())
		}
	}
	if bytes.Contains(output.Bytes(), []byte("private SQL")) {
		t.Fatal("SQL error message leaked into request logs")
	}
}

func TestAppMiddlewareLogsResolvedIdentity(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	router := gin.New()
	router.Use(MiddlewareForApp(logger, "journal")...)
	router.Use(func(context *gin.Context) {
		context.Set(OperationIDContextKey, "organizeJournal")
		context.Next()
	})
	router.GET("/v1/journal/:id", func(context *gin.Context) { context.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, "https://api.journal.test/v1/journal/entry-1", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	logged := output.String()
	for _, expected := range []string{
		`"app_id":"journal"`, `"host":"api.journal.test"`,
		`"operation_id":"organizeJournal"`, `"path":"/v1/journal/entry-1"`,
	} {
		if !bytes.Contains([]byte(logged), []byte(expected)) {
			t.Fatalf("log is missing %s: %s", expected, logged)
		}
	}
}
