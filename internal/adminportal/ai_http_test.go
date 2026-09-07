package adminportal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/contracts"
)

type aiAuthFixture struct {
	adminauth.Repository
	user adminauth.User
}

func (r *aiAuthFixture) UserByID(context.Context, string) (adminauth.User, bool, error) {
	return r.user, true, nil
}
func (r *aiAuthFixture) AppendAudit(context.Context, adminauth.AuditEvent) error { return nil }

type aiStoreFixture struct {
	aiconfig.Store
	draft   *aiconfig.Revision
	current *aiconfig.Revision
	writes  int
	fail    error
}

func (s *aiStoreFixture) Current(context.Context, contracts.Operation) (*aiconfig.Revision, error) {
	return s.current, s.fail
}
func (s *aiStoreFixture) Get(_ context.Context, op contracts.Operation, id string) (*aiconfig.Revision, error) {
	if s.draft == nil || s.draft.ID != id || s.draft.Operation != op {
		return nil, aiconfig.ErrNotFound
	}
	return s.draft, s.fail
}
func (s *aiStoreFixture) Replay(context.Context, aiconfig.Mutation, string, any) (*aiconfig.Revision, error) {
	return nil, s.fail
}
func (s *aiStoreFixture) Draft(_ context.Context, r aiconfig.Revision, _ aiconfig.Mutation) (aiconfig.Revision, error) {
	s.writes++
	s.draft = &r
	return r, s.fail
}
func (s *aiStoreFixture) Publish(_ context.Context, p aiconfig.Publication, _ aiconfig.Mutation, now time.Time) (aiconfig.Revision, error) {
	s.writes++
	r := *s.draft
	r.PublishedAt = &now
	s.current = &r
	return r, s.fail
}
func (s *aiStoreFixture) HistoryPage(context.Context, contracts.Operation, string) (aiconfig.HistoryPage, error) {
	return aiconfig.HistoryPage{}, s.fail
}

func TestAIHTTPAuthorizationAndPublicationPreview(t *testing.T) {
	now := time.Now().UTC()
	repo := &aiAuthFixture{user: adminauth.User{ID: uuid.NewString(), Role: adminauth.RoleAdmin, Status: adminauth.UserStatusActive}}
	sessions := adminauth.NewMemoryStateStore(func() time.Time { return now })
	auth, err := adminauth.NewService(repo, sessions, adminauth.Config{RPID: "admin.example.test", Origin: "https://admin.example.test", AppIDs: []string{"health"}}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	session := adminauth.Session{UserID: repo.user.ID, CSRFToken: "fixture-csrf", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), ReauthenticatedAt: now}
	putSession := func() {
		t.Helper()
		if err := sessions.PutSession(context.Background(), adminauth.TokenHash(token), session, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	putSession()
	store := &aiStoreFixture{}
	ep := inventoryFixture{}.ep
	ep.ID = "ep-health"
	ep.Status = "Running"
	ep.Model.FoundationModel.Name = "doubao-seed-2-0-mini"
	ep.Model.FoundationModel.Version = "260428"
	s := &Server{auth: auth, now: func() time.Time { return now }, config: Config{WritesEnabled: false, PreviewSigningKey: bytes.Repeat([]byte{1}, 32), AI: &AIConfig{WritesEnabled: true, Store: store, Inventory: inventoryFixture{ep}, TimeoutSeconds: 90, Endpoints: map[contracts.Operation]string{contracts.OperationMealDecision: ep.ID}}}}
	router := gin.New()
	router.Use(limitAdminRequestBody())
	adminhttpapi.RegisterHandlers(router, &adminHTTPServer{Server: s, Service: auth})
	call := func(method, path string, body any, authenticated, csrf bool, key string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.AddCookie(&http.Cookie{Name: "__Host-tellyouwhat_admin_session", Value: token})
		}
		if csrf {
			req.Header.Set("Origin", "https://admin.example.test")
			req.Header.Set("X-Admin-CSRF", session.CSRFToken)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	draft := map[string]any{"operation": "meal_decision", "baseVersion": "", "policy": contracts.ExecutionPolicy{Endpoint: ep.ID, ReasoningEffort: "low", TimeoutSeconds: 90}}
	assert := func(res *httptest.ResponseRecorder, status int) {
		t.Helper()
		if res.Code != status {
			t.Fatalf("status=%d want=%d body=%s", res.Code, status, res.Body.String())
		}
	}

	for _, path := range []string{"/api/v1/ai/rolling/preview", "/api/v1/ai/rolling/commands"} {
		assert(call("POST", path, map[string]any{}, false, true, uuid.NewString()), 401)
		repo.user.Role = adminauth.RoleOperator
		assert(call("POST", path, map[string]any{}, true, true, uuid.NewString()), 403)
		repo.user.Role = adminauth.RoleAdmin
		assert(call("POST", path, map[string]any{}, true, false, uuid.NewString()), 403)
		session.ReauthenticatedAt = time.Time{}
		putSession()
		assert(call("POST", path, map[string]any{}, true, true, uuid.NewString()), 401)
		session.ReauthenticatedAt = now
		putSession()
		s.config.AI.WritesEnabled = false
		assert(call("POST", path, map[string]any{}, true, true, uuid.NewString()), 503)
		s.config.AI.WritesEnabled = true
		assert(call("POST", path, map[string]any{}, true, true, uuid.NewString()), 422)
	}
	assert(call("POST", "/api/v1/ai/health/drafts", draft, false, true, uuid.NewString()), 401)
	repo.user.Role = adminauth.RoleOperator
	assert(call("GET", "/api/v1/ai/health", nil, true, false, ""), 403)
	assert(call("POST", "/api/v1/ai/health/drafts", draft, true, true, uuid.NewString()), 403)
	repo.user.Role = adminauth.RoleAdmin
	assert(call("POST", "/api/v1/ai/health/drafts", draft, true, false, uuid.NewString()), 403)
	session.ReauthenticatedAt = time.Time{}
	putSession()
	assert(call("POST", "/api/v1/ai/health/drafts", draft, true, true, uuid.NewString()), 401)
	session.ReauthenticatedAt = now
	putSession()
	s.config.AI.WritesEnabled = false
	assert(call("POST", "/api/v1/ai/health/drafts", draft, true, true, uuid.NewString()), 503)
	s.config.AI.WritesEnabled = true
	assert(call("POST", "/api/v1/ai/health/drafts", draft, true, true, ""), 400)
	if store.writes != 0 {
		t.Fatal("unauthorized write reached store")
	}
	assert(call("POST", "/api/v1/ai/health/drafts", draft, true, true, uuid.NewString()), 200)
	previewPath := "/api/v1/ai/health/meal_decision/revisions/" + store.draft.ID
	response := call("GET", previewPath, nil, true, false, "")
	assert(response, 200)
	var preview aiPreview
	if json.Unmarshal(response.Body.Bytes(), &preview) != nil || !preview.CanPublish || preview.PreviewToken == "" {
		t.Fatal("missing server preview")
	}
	pub := map[string]any{"operation": "meal_decision", "revision": store.draft.ID, "baseVersion": "", "previewToken": "forged"}
	assert(call("POST", "/api/v1/ai/health/publish", pub, true, true, uuid.NewString()), 409)
	pub["previewToken"] = preview.PreviewToken
	ep.Name = "externally renamed"
	s.config.AI.Inventory = inventoryFixture{ep}
	assert(call("POST", "/api/v1/ai/health/publish", pub, true, true, uuid.NewString()), 409)
	ep.Name = ""
	s.config.AI.Inventory = inventoryFixture{ep}
	now = now.Add(5 * time.Minute)
	session.ReauthenticatedAt = now
	putSession()
	assert(call("POST", "/api/v1/ai/health/publish", pub, true, true, uuid.NewString()), 409)
	response = call("GET", previewPath, nil, true, false, "")
	assert(response, 200)
	json.Unmarshal(response.Body.Bytes(), &preview)
	pub["previewToken"] = preview.PreviewToken
	assert(call("POST", "/api/v1/ai/health/publish", pub, true, true, uuid.NewString()), 200)
	assert(call("GET", "/api/v1/ai/health/meal_text_capture/revisions/"+store.draft.ID, nil, true, false, ""), 404)
	store.fail = errors.New("db private diagnostic")
	response = call("POST", "/api/v1/ai/health/drafts", draft, true, true, uuid.NewString())
	assert(response, 503)
	if bytes.Contains(response.Body.Bytes(), []byte("private")) {
		t.Fatal("leaked internal error")
	}
}
