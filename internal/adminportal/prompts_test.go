package adminportal

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"github.com/tellyouwhat/backend/internal/testutil"
)

func TestPromptHTTPPublicationLifecycle(t *testing.T) {
	t.Setenv("AI_PROJECT_MONTHLY_BUDGET_CNY", "100")
	db := testutil.MySQL(t)
	ctx := context.Background()
	now := time.Now().UTC()
	repo := &aiAuthFixture{user: adminauth.User{ID: uuid.NewString(), Role: adminauth.RoleAdmin, Status: adminauth.UserStatusActive}}
	if _, err := db.Exec(`INSERT INTO admin_users(id,webauthn_id,display_name,role) VALUES(?,?,?,'admin')`, repo.user.ID, []byte(repo.user.ID), "Fixture"); err != nil {
		t.Fatal(err)
	}
	store := &promptconfig.Store{DB: db}
	scope := "health:meal_text_capture"
	policy := promptconfig.Policy{}
	if err := store.Initialize(ctx, scope, policy, now); err != nil {
		t.Fatal(err)
	}
	current, err := store.Current(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	sessions := adminauth.NewMemoryStateStore(func() time.Time { return now })
	auth, err := adminauth.NewService(repo, sessions, adminauth.Config{RPID: "admin.example.test", Origin: "https://admin.example.test", AppIDs: []string{"health", "journal"}}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	session := adminauth.Session{UserID: repo.user.ID, CSRFToken: "fixture-csrf", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), ReauthenticatedAt: now}
	put := func() {
		t.Helper()
		if err := sessions.PutSession(ctx, adminauth.TokenHash(token), session, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	put()
	s := &Server{auth: auth, now: func() time.Time { return now }, config: Config{Prompts: store, AI: &AIConfig{WritesEnabled: true}, PreviewSigningKey: bytes.Repeat([]byte{1}, 32)}}
	router := gin.New()
	router.Use(limitAdminRequestBody())
	adminhttpapi.RegisterHandlers(router, &adminHTTPServer{Server: s, Service: auth})
	call := func(method, path string, body any, login, csrf bool, key string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if login {
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
	check := func(r *httptest.ResponseRecorder, status int) {
		t.Helper()
		if r.Code != status {
			t.Fatalf("status=%d want=%d: %s", r.Code, status, r.Body.String())
		}
	}
	root := "/api/v1/ai/prompts"
	for _, path := range []string{"?scope=" + scope, "/history?scope=" + scope, "/revisions/" + current.ID} {
		check(call("GET", root+path, nil, false, false, ""), 401)
		repo.user.Role = adminauth.RoleOperator
		check(call("GET", root+path, nil, true, false, ""), 403)
		repo.user.Role = adminauth.RoleAdmin
		check(call("GET", root+path, nil, true, false, ""), 200)
	}
	policy.SystemPrompt = "附加系统提示词"
	input := promptconfig.DraftInput{Scope: scope, BaseVersion: current.ID, Policy: policy}
	check(call("POST", root+"/drafts", input, true, false, uuid.NewString()), 403)
	s.config.AI.WritesEnabled = false
	check(call("POST", root+"/drafts", input, true, true, uuid.NewString()), 503)
	s.config.AI.WritesEnabled = true
	check(call("POST", root+"/drafts", input, true, true, ""), 400)
	session.ReauthenticatedAt = time.Time{}
	put()
	res := call("POST", root+"/drafts", input, true, true, uuid.NewString())
	check(res, 200)
	var draft promptconfig.Revision
	if err = json.Unmarshal(res.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	preview := func() string {
		t.Helper()
		res := call("GET", root+"/revisions/"+draft.ID, nil, true, false, "")
		check(res, 200)
		var out struct {
			PreviewToken string `json:"previewToken"`
			CanPublish   bool   `json:"canPublish"`
		}
		json.Unmarshal(res.Body.Bytes(), &out)
		if !out.CanPublish || out.PreviewToken == "" {
			t.Fatal("missing preview")
		}
		return out.PreviewToken
	}
	body := map[string]any{"scope": scope, "revision": draft.ID, "baseVersion": current.ID, "previewToken": preview()}
	check(call("POST", root+"/publish", body, true, true, uuid.NewString()), 401)
	now = now.Add(6 * time.Minute)
	session.ReauthenticatedAt = now
	put()
	check(call("POST", root+"/publish", body, true, true, uuid.NewString()), 409)
	body["previewToken"] = preview() + "tampered"
	check(call("POST", root+"/publish", body, true, true, uuid.NewString()), 409)
	body["previewToken"] = preview()
	key := uuid.NewString()
	res = call("POST", root+"/publish", body, true, true, key)
	check(res, 200)
	now = now.Add(6 * time.Minute)
	session.ReauthenticatedAt = now
	put()
	replay := call("POST", root+"/publish", body, true, true, key)
	check(replay, 200)
	if replay.Body.String() != res.Body.String() {
		t.Fatal("publication replay changed result")
	}
	check(call("POST", root+"/drafts", input, true, true, uuid.NewString()), 409)
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM admin_audit_events WHERE action='prompts.publish'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit count=%d err=%v", count, err)
	}
}
