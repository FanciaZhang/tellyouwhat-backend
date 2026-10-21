package adminportal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
)

func TestVoiceCatalogDraftBodyIsBoundedWithoutChangingOtherAdminLimits(t *testing.T) {
	router := gin.New()
	router.Use(limitAdminRequestBody())
	for _, path := range []string{"/api/v1/journal/read-aloud/catalog/drafts", "/api/v1/ai/prompts/drafts"} {
		router.POST(path, func(c *gin.Context) {
			if _, err := io.Copy(io.Discard, c.Request.Body); err != nil {
				c.Status(413)
			} else {
				c.Status(204)
			}
		})
	}
	for _, test := range []struct {
		path   string
		size   int64
		status int
	}{
		{"/api/v1/journal/read-aloud/catalog/drafts", 2 << 20, 204},
		{"/api/v1/journal/read-aloud/catalog/drafts", 33 << 20, 413},
		{"/api/v1/ai/prompts/drafts", 2 << 20, 413},
	} {
		reader := io.LimitReader(&voiceBodyFixture{}, test.size)
		r := httptest.NewRequest("POST", test.path, reader)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatal("wrong request boundary", test.path, test.size, w.Code)
		}
	}
}

type voiceBodyFixture struct{}

func (*voiceBodyFixture) Read(p []byte) (int, error) { return len(p), nil }

func TestVoiceCatalogAdminStartsWithoutAnAppleOfferClientAndKeepsAuthentication(t *testing.T) {
	now := time.Now().UTC()
	auth, err := adminauth.NewService(&aiAuthFixture{}, adminauth.NewMemoryStateStore(time.Now),
		adminauth.Config{RPID: "admin.example.test", Origin: "https://admin.example.test", AppIDs: []string{"journal"}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(auth, nil, NewMySQLOperationStore(nil), NewMySQLMetricsReader(nil), Config{
		Apps: []AdminApp{{ID: "journal", DisplayName: "手记测试"}}, PreviewSigningKey: make([]byte, 32),
		VoiceCatalog: &readaloud.CatalogStore{},
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/journal/read-aloud/catalog", nil))
	if response.Code != 401 {
		t.Fatal("voice catalog no longer requires admin authentication", response.Code)
	}
	for app, status := range map[string]int{"journal": 503, "health": 404} {
		response := httptest.NewRecorder()
		if _, _, ok := server.offerManager(response, app); ok || response.Code != status {
			t.Fatal("disabled or unselected offer service was accepted", app, response.Code)
		}
	}
}

func TestVoiceCatalogHTTPAuthCSRFWriteGateAndNoClientVerification(t *testing.T) {
	now := time.Now().UTC()
	repo := &aiAuthFixture{user: adminauth.User{ID: uuid.NewString(), Role: adminauth.RoleAdmin, Status: adminauth.UserStatusActive}}
	sessions := adminauth.NewMemoryStateStore(func() time.Time { return now })
	auth, err := adminauth.NewService(repo, sessions, adminauth.Config{RPID: "admin.example.test", Origin: "https://admin.example.test", AppIDs: []string{"journal"}}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	session := adminauth.Session{UserID: repo.user.ID, CSRFToken: "fixture-csrf", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), ReauthenticatedAt: now}
	if err := sessions.PutSession(context.Background(), adminauth.TokenHash(token), session, time.Hour); err != nil {
		t.Fatal(err)
	}
	s := &Server{auth: auth, now: func() time.Time { return now }, config: Config{VoiceCatalog: &readaloud.CatalogStore{}, VoiceProbe: &readaloud.CatalogProbe{}, VoiceWritesEnabled: true}}
	router := gin.New()
	router.Use(limitAdminRequestBody())
	adminhttpapi.RegisterHandlers(router, &adminHTTPServer{Server: s, Service: auth})
	base := "/api/v1/journal/read-aloud/catalog"
	call := func(method, path string, body any, authenticated bool, csrf string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://admin.example.test")
		req.Header.Set("X-Admin-CSRF", csrf)
		req.Header.Set("Idempotency-Key", uuid.NewString())
		if authenticated {
			req.AddCookie(&http.Cookie{Name: "__Host-tellyouwhat_admin_session", Value: token})
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	if r := call("GET", base, nil, false, ""); r.Code != 401 {
		t.Fatal("anonymous catalog management allowed", r.Code)
	}
	if r := call("POST", base+"/drafts", map[string]any{}, true, "wrong"); r.Code != 403 {
		t.Fatal("CSRF bypass", r.Code, r.Body.String())
	}
	s.config.VoiceWritesEnabled = false
	if r := call("POST", base+"/drafts", map[string]any{}, true, session.CSRFToken); r.Code != 503 {
		t.Fatal("write gate bypass", r.Code)
	}
	s.config.VoiceWritesEnabled = true
	if r := call("POST", base+"/probe", map[string]any{"id": "voice", "verification": map[string]any{"policySHA256": "forged"}}, true, session.CSRFToken); r.Code != 400 {
		t.Fatal("client probe evidence accepted", r.Code, r.Body.String())
	}
	session.ReauthenticatedAt = now.Add(-time.Hour)
	if err := sessions.PutSession(context.Background(), adminauth.TokenHash(token), session, time.Hour); err != nil {
		t.Fatal(err)
	}
	if r := call("POST", base+"/publish", map[string]any{}, true, session.CSRFToken); r.Code != 401 || !strings.Contains(r.Body.String(), "reauthentication_required") {
		t.Fatal("publication skipped fresh authentication", r.Code)
	}
	repo.user.Role = adminauth.RoleOperator
	if r := call("GET", base, nil, true, session.CSRFToken); r.Code != 403 {
		t.Fatal("operator acquired AI configuration permission", r.Code)
	}
}
