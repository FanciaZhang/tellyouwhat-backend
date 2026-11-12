package adminportal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"github.com/tellyouwhat/backend/internal/prompteval"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVoiceDebugOperatorIsScopedToOwnedJournalTrials(t *testing.T) {
	t.Setenv("AI_PROJECT_MONTHLY_BUDGET_CNY", "100")
	db := testutil.MySQL(t)
	ctx := context.Background()
	now := time.Now().UTC()
	repo := &aiAuthFixture{user: adminauth.User{ID: uuid.NewString(), Role: adminauth.RoleAdmin, Status: adminauth.UserStatusActive}}
	if _, err := db.Exec(`INSERT INTO admin_users(id,webauthn_id,display_name,role) VALUES(?,?,?,'admin')`, repo.user.ID, []byte(repo.user.ID), "Fixture"); err != nil {
		t.Fatal(err)
	}
	store := &promptconfig.Store{DB: db}
	scope := "journal"
	policy := promptconfig.Defaults("ep-test", "ep-test", "ep-test", 90)["journal"]
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
	cipher, _ := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	evaluations := &prompteval.Store{DB: db, Cipher: cipher, Limits: costcontrol.Limits{MonthlyBudgetNanos: 100e9, MaxConcurrent: 2, LeaseDuration: time.Hour}}
	s := &Server{auth: auth, now: func() time.Time { return now }, config: Config{Prompts: store, Evaluations: evaluations, EvaluationSpeechPrice: costcontrol.DurationPrice{NanosPerHour: 4500000000}, AI: &AIConfig{WritesEnabled: true, Inventory: evalInventory{}}, PreviewSigningKey: bytes.Repeat([]byte{1}, 32)}}
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

	root := "/api/v1/ai/evaluations"
	sample := prompteval.Sample{ID: uuid.NewString(), Name: "Only admin owns this audio", Kind: "voice_transcription", VoiceDebug: true, Voice: &voice.Snapshot{}, Audio: make([]byte, 32000), Expected: []prompteval.Check{}}
	check(call("POST", root+"/samples", sample, true, true, ""), 200)
	input := evaluationInput{Mode: prompteval.VoiceDebugMode, SampleIDs: []string{sample.ID}}
	input.Candidates = append(input.Candidates, struct {
		Revision      string                   `json:"revision"`
		Label         string                   `json:"label"`
		ModelOverride *promptconfig.Parameters `json:"modelOverride,omitempty"`
	}{Revision: current.ID, Label: "调试"})
	var preview struct {
		PreviewToken string `json:"previewToken"`
	}
	res := call("POST", root+"/preview", input, true, true, "")
	check(res, 200)
	json.Unmarshal(res.Body.Bytes(), &preview)
	raw, _ := json.Marshal(input)
	var start map[string]any
	json.Unmarshal(raw, &start)
	start["previewToken"] = preview.PreviewToken
	res = call("POST", root+"/runs", start, true, true, uuid.NewString())
	check(res, 200)
	var foreign prompteval.Run
	json.Unmarshal(res.Body.Bytes(), &foreign)
	owner := repo.user.ID
	repo.user = adminauth.User{ID: uuid.NewString(), Role: adminauth.RoleOperator, Status: adminauth.UserStatusActive, AppIDs: []string{"journal"}}
	session.UserID = repo.user.ID
	put()
	if _, err := db.Exec(`INSERT INTO admin_users(id,webauthn_id,display_name,role)VALUES(?,?,?,'operator')`, repo.user.ID, []byte(repo.user.ID), "Journal operator"); err != nil {
		t.Fatal(err)
	}
	check(call("GET", "/api/v1/ai/voice-debug/config", nil, true, false, ""), 200)
	check(call("GET", "/api/v1/ai/prompts?scope=journal", nil, true, false, ""), 403)
	check(call("POST", "/api/v1/ai/prompts/publish", map[string]any{}, true, true, uuid.NewString()), 403)
	check(call("POST", "/api/v1/ai/voice-debug/drafts", promptconfig.DraftInput{Scope: "health:meal_text_capture", BaseVersion: current.ID, Policy: policy}, true, true, uuid.NewString()), 422)
	check(call("GET", root+"/samples/"+sample.ID, nil, true, false, ""), 404)
	check(call("GET", root+"/runs/"+foreign.ID, nil, true, false, ""), 404)
	check(call("GET", root+"/runs/"+foreign.ID+"/items/0", nil, true, false, ""), 404)
	check(call("POST", root+"/runs/"+foreign.ID+"/cancel", nil, true, true, ""), 404)
	own := sample
	own.ID = uuid.NewString()
	check(call("POST", root+"/samples", own, true, false, ""), 403)
	check(call("POST", root+"/samples", own, true, true, ""), 200)
	check(call("GET", root+"/samples/"+own.ID, nil, true, false, ""), 200)
	input.SampleIDs = []string{own.ID}
	input.Mode = ""
	check(call("POST", root+"/preview", input, true, true, ""), 422)
	input.Mode = prompteval.VoiceDebugMode
	check(call("POST", root+"/preview", input, true, true, ""), 200)
	res = call("POST", "/api/v1/ai/voice-debug/drafts", promptconfig.DraftInput{Scope: "journal", BaseVersion: current.ID, Policy: policy}, true, true, uuid.NewString())
	check(res, 200)
	var draft promptconfig.Revision
	json.Unmarshal(res.Body.Bytes(), &draft)
	if draft.CreatedBy != repo.user.ID {
		t.Fatal("wrong draft owner")
	}
	input.Candidates[0].Revision = draft.ID
	check(call("POST", root+"/preview", input, true, true, ""), 200)
	repo.user.AppIDs = []string{"health"}
	check(call("GET", "/api/v1/ai/voice-debug/config", nil, true, false, ""), 403)
	repo.user.AppIDs = []string{"journal"}
	res = call("GET", root+"/runs", nil, true, false, "")
	check(res, 200)
	if bytes.Contains(res.Body.Bytes(), []byte(foreign.ID)) {
		t.Fatal("foreign run listed", owner)
	}
}
