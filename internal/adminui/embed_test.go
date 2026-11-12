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

func TestVoiceWorkbenchScriptIsShippedWithAdminShell(t *testing.T) {
	handler := testRouter()
	for _, path := range []string{"/", "/voice-lab.js", "/voice-lab-drafts.js"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), map[string]string{"/": `/voice-lab.js`, "/voice-lab.js": "openVoiceLab", "/voice-lab-drafts.js": "voiceDrafts"}[path]) {
			t.Fatal("workbench asset missing from compiled admin", path, response.Code)
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
	router.NoRoute(func(context *gin.Context) { Render(context, false) })
	return router
}

func TestTestEnvironmentIsVisibleBeforeLoginAndDoesNotChangeProduction(t *testing.T) {
	router := gin.New()
	router.NoRoute(func(c *gin.Context) { Render(c, true) })
	for _, path := range []string{"/", "/setup", "/enroll"} {
		result := httptest.NewRecorder()
		router.ServeHTTP(result, httptest.NewRequest("GET", path, nil))
		for _, text := range []string{"Tellyouwhat 测试管理后台", "<h1>测试管理后台</h1>", "Tellyouwhat · 测试环境"} {
			if !strings.Contains(result.Body.String(), text) {
				t.Fatal("test environment missing", path, text)
			}
		}
	}
	result := httptest.NewRecorder()
	testRouter().ServeHTTP(result, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(result.Body.String(), "测试管理后台") {
		t.Fatal("development label leaked into production")
	}
}
