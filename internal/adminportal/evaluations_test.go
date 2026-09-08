package adminportal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/prompteval"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
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

func TestEvaluationHTTPPreviewReplayAndPublicationGate(t *testing.T) {
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
	check(call("GET", root+"/samples", nil, false, false, ""), 401)
	repo.user.Role = adminauth.RoleOperator
	check(call("GET", root+"/samples", nil, true, false, ""), 403)
	repo.user.Role = adminauth.RoleAdmin
	check(call("GET", root+"/samples", nil, true, false, ""), 200)
	input := evaluationInput{SampleIDs: []string{prompteval.Builtins()[0].ID, prompteval.Builtins()[1].ID, prompteval.Builtins()[2].ID}}
	raw, _ := json.Marshal(map[string]any{"sampleIDs": input.SampleIDs, "candidates": []any{map[string]any{"revision": current.ID, "label": "当前版"}}})
	_ = json.Unmarshal(raw, &input)
	check(call("POST", root+"/preview", input, true, false, ""), 403)
	session.ReauthenticatedAt = time.Time{}
	put()
	res := call("POST", root+"/preview", input, true, true, "")
	check(res, 200)
	var preview struct {
		Plan  prompteval.Plan `json:"plan"`
		Token string          `json:"previewToken"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &preview)
	if preview.Token == "" || preview.Plan.MaximumCalls != 6 {
		t.Fatal("preview did not freeze scale")
	}
	start := map[string]any{}
	_ = json.Unmarshal(raw, &start)
	start["previewToken"] = preview.Token
	key := uuid.NewString()
	res = call("POST", root+"/runs", start, true, true, key)
	check(res, 200)
	var run prompteval.Run
	_ = json.Unmarshal(res.Body.Bytes(), &run)
	if run.Plan.Samples[0].Organize != nil || len(run.Plan.Requests) > 0 {
		t.Fatal("poll response includes raw samples")
	}
	now = now.Add(6 * time.Minute)
	res = call("POST", root+"/runs", start, true, true, key)
	check(res, 200)
	check(call("POST", root+"/runs", start, true, true, uuid.NewString()), 409)
	check(call("POST", root+"/runs/"+run.ID+"/cancel", nil, true, true, ""), 200)
	sample := prompteval.Builtins()[0]
	sample.ID = uuid.NewString()
	check(call("POST", root+"/samples", sample, true, true, ""), 200)
	check(call("POST", root+"/samples", sample, true, true, ""), 200)
	check(call("DELETE", root+"/samples/"+sample.ID, nil, true, true, ""), 200)
	check(call("GET", root+"/samples/"+sample.ID, nil, true, false, ""), 404)
	// The same revision under a different model receives a distinct comparison ID.
	in := input
	parameter := preview.Plan.Judge
	parameter.Model = "other-1"
	parameter.FoundationModel = "other"
	parameter.ModelVersion = "1"
	in.Candidates[0].ModelOverride = &parameter
	res = call("POST", root+"/preview", in, true, true, "")
	check(res, 200)
	_ = json.Unmarshal(res.Body.Bytes(), &preview)
	if preview.Plan.Candidates[0].Revision.ID == current.ID || preview.Plan.Candidates[0].SourceRevision != current.ID {
		t.Fatal("model override can certify original revision")
	}
	// Draft publication remains gated even when no passkey is needed for evaluation.
	draft, err := store.Draft(ctx, promptconfig.DraftInput{Scope: scope, BaseVersion: current.ID, Policy: policy}, promptconfig.Mutation{Actor: repo.user.ID, Key: uuid.NewString(), RequestID: uuid.NewString()}, now)
	if err != nil {
		t.Fatal(err)
	}
	res = call("GET", "/api/v1/ai/prompts/revisions/"+draft.ID, nil, true, false, "")
	check(res, 200)
	var publication struct {
		CanPublish bool
		Blockers   []string
	}
	_ = json.Unmarshal(res.Body.Bytes(), &publication)
	if publication.CanPublish || len(publication.Blockers) == 0 {
		t.Fatal("untested draft can publish")
	}
}

type evalInventory struct{}

func (evalInventory) Models(context.Context) ([]arkcontrol.Model, error) { return nil, nil }
func (evalInventory) Endpoint(context.Context, string) (arkcontrol.Endpoint, error) {
	var ep arkcontrol.Endpoint
	ep.Status = "Running"
	ep.Model.FoundationModel.Name = "synthetic"
	ep.Model.FoundationModel.Version = "1"
	return ep, nil
}
func (evalInventory) Versions(_ context.Context, name string) ([]arkcontrol.Version, error) {
	return []arkcontrol.Version{{Name: name, Version: "1", ModelID: name + "-1", Status: "Published", Domains: []string{"LLM"}}}, nil
}
func (evalInventory) ModelActivations(_ context.Context, names []string) ([]arkcontrol.Activation, error) {
	return []arkcontrol.Activation{{Name: names[0], State: "Available", Charges: []arkcontrol.ChargeItem{{Type: "InferencePrompt", Unit: "百万tokens", Price: json.Number("1")}, {Type: "InferenceCompletion", Unit: "百万tokens", Price: json.Number("2")}}}}, nil
}
