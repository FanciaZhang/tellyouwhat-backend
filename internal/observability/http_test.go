package observability

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
