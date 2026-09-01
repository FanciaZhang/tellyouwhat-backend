package adminui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHandlerServesLoginAndSetupShell(t *testing.T) {
	handler := testRouter()
	for _, path := range []string{"/", "/setup", "/enroll"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "管理后台") {
			t.Fatalf("%s returned %d: %s", path, response.Code, response.Body.String())
		}
	}
}

func TestHandlerUsesPasskeysWithoutPasswordRecoverySurface(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	testRouter().ServeHTTP(response, request)
	body := response.Body.String()
	for _, required := range []string{"使用通行密钥登录", "后台人员", "操作记录"} {
		if !strings.Contains(body, required) {
			t.Fatalf("admin shell is missing %q", required)
		}
	}
	for _, forbidden := range []string{`type="password"`, "一次性恢复码", "recovery-code"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("admin shell still contains %q", forbidden)
		}
	}
}

func TestHandlerDoesNotServeUnknownOrTraversalPaths(t *testing.T) {
	handler := testRouter()
	for _, path := range []string{"/missing.js", "/../go.mod"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d", path, response.Code)
		}
	}
}

func testRouter() *gin.Engine {
	router := gin.New()
	router.NoRoute(Handle)
	return router
}
