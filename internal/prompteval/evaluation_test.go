package prompteval

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPlan(t *testing.T) Plan {
	t.Helper()
	p := promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 1e9, OutputNanosPerMillionTokens: 2e9}
	p.Journal.Organize.Lite.Price = &price
	p.Journal.Organize.Pro.Price = &price
	p.Journal.Voice.Parameters.Price = &price
	plan, err := Prepare(Builtins(), []Candidate{{Revision: promptconfig.Revision{ID: uuid.NewString(), Scope: "journal", Policy: p}, Label: "草稿"}}, p.Journal.Organize.Pro, costcontrol.DurationPrice{NanosPerHour: 4500000000})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
func testStore(t *testing.T) (Store, aiconfig.Mutation) {
	t.Helper()
	db := testutil.MySQL(t)
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	m := aiconfig.Mutation{Actor: uuid.NewString(), Key: uuid.NewString(), RequestID: uuid.NewString()}
	now := time.Now()
	_, err = db.Exec(`INSERT INTO admin_users(id,webauthn_id,display_name,role,status,created_at,updated_at) VALUES(?,UNHEX(REPLACE(UUID(),'-','')),'Evaluation tester','admin','active',?,?)`, m.Actor, now, now)
	if err != nil {
		t.Fatal(err)
	}
	return Store{DB: db, Cipher: cipher, Limits: costcontrol.Limits{MonthlyBudgetNanos: 1000e9, MaxConcurrent: 4, LeaseDuration: time.Hour}}, m
}
func TestSyntheticSamplesAndFrozenProductionRequests(t *testing.T) {
	p := testPlan(t)
	if len(p.Samples) != 7 || p.MaximumCalls != 14 {
		t.Fatal("scale", p.MaximumCalls)
	}
	for i, s := range p.Samples {
		if err := s.Validate(); err != nil {
			t.Fatal(i, err)
		}
	}
	request, _, err := provider.PrepareOrganize(promptconfig.WithRevision(context.Background(), p.Candidates[0].Revision), *p.Samples[0].Organize, false, provider.Config{})
	if err != nil || !bytes.Equal(p.Requests[0], request.Body) {
		t.Fatal("production preview diverged", err)
	}
}
func TestMySQLBudgetReplayCancellationAndRetention(t *testing.T) {
	s, m := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	p := testPlan(t)
	sample := p.Samples[0]
	sample.ID = uuid.NewString()
	if err := s.SaveSample(ctx, sample, m.Actor, now); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := s.DB.QueryRow(`SELECT payload FROM prompt_eval_samples WHERE id=?`, sample.ID).Scan(&raw); err != nil || bytes.Contains(raw, []byte(sample.Name)) {
		t.Fatal("sample encryption", err)
	}
	run, err := s.Start(ctx, p, m, now)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Start(ctx, p, m, now)
	if err != nil || replay.ID != run.ID {
		t.Fatal("replay", err)
	}
	changed := p
	changed.MaximumCalls++
	if _, err = s.Start(ctx, changed, m, now); !errors.Is(err, ErrConflict) {
		t.Fatal("idempotency mismatch", err)
	}
	var charged int64
	s.DB.QueryRow(`SELECT charged_nanos FROM ai_cost_months`).Scan(&charged)
	if charged != p.ReservedNanos {
		t.Fatal("hold not charged", charged)
	}
	tooLarge := p
	tooLarge.ReservedNanos = s.Limits.MonthlyBudgetNanos
	next := m
	next.Key = uuid.NewString()
	if _, err = s.Start(ctx, tooLarge, next, now); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatal("budget admission", err)
	}
	controller, _ := costcontrol.New(mysqlstore.NewCostControlStore(s.DB), s.Limits, time.Now)
	if _, err = controller.Reserve(ctx, "journal", "user_operation", "ark", s.Limits.MonthlyBudgetNanos); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatal("ordinary calls must include holds", err)
	}
	if err = s.Cancel(ctx, run.ID, now); err != nil {
		t.Fatal(err)
	}
	s.DB.QueryRow(`SELECT charged_nanos FROM ai_cost_months`).Scan(&charged)
	if charged != 0 {
		t.Fatal("cancel did not return unused budget", charged)
	}
	if err = s.Cancel(ctx, run.ID, now); err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx, now.Add(Retention+time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(ctx, run.ID, now.Add(Retention+time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatal("retention", err)
	}
	if _, err = s.Sample(ctx, sample.ID, now.Add(Retention+time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatal("sample retention", err)
	}
}
func TestMySQLLeaseRecoveryAndConcurrency(t *testing.T) {
	s, m := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	p := testPlan(t)
	run, err := s.Start(ctx, p, m, now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan Claim, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, e := s.Claim(ctx, now); e == nil {
				claims <- c
			}
		}()
	}
	wg.Wait()
	close(claims)
	all := []Claim{}
	for c := range claims {
		all = append(all, c)
	}
	if len(all) != 2 {
		t.Fatal("global concurrency", len(all))
	}
	if err = s.Recover(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveResult(ctx, all[0], ItemResult{}, true); !errors.Is(err, ErrConflict) {
		t.Fatal("expired lease accepted", err)
	}
	next, err := s.Claim(ctx, now.Add(time.Minute))
	if err != nil || next.Index < 2 {
		t.Fatal("pending work not recovered", err, next.Index)
	}
	r, err := s.Get(ctx, run.ID, now)
	if err != nil || r.Items[0].Status != "interrupted" || r.Items[1].Status != "interrupted" {
		t.Fatal("uncertain work repeated", err)
	}
	_ = s.Cancel(ctx, run.ID, now)
	if s.Heartbeat(ctx, next, now) {
		t.Fatal("cancelled heartbeat accepted")
	}
}
func TestMySQLEngineTraceJudgeFailureAndPublicationGate(t *testing.T) {
	s, m := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	p := testPlan(t)
	requests := map[string]int{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		name := body["text"].(map[string]any)["format"].(map[string]any)["name"].(string)
		mu.Lock()
		requests[name]++
		mu.Unlock()
		if strings.Contains(name, "evaluation") {
			http.Error(w, "synthetic judge failure", 503)
			return
		}
		value := `{"tags":[],"existingBookRecommendations":[],"newBookSuggestions":[]}`
		if body["model"] == "voice" {
			value = `{"baseRevision":1,"transcriptRevision":1,"patches":[],"questions":[]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "model": "actual-" + body["model"].(string), "usage": map[string]int{"input_tokens": 100, "output_tokens": 30}, "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": value}}}}})
	}))
	defer server.Close()
	run, err := s.Start(ctx, p, m, now)
	if err != nil {
		t.Fatal(err)
	}
	e := Engine{Store: s, Provider: provider.Config{BaseURL: server.URL, APIKey: "test"}, Price: *p.Judge.Price}
	for range len(p.Samples) {
		claim, err := s.Claim(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		e.Process(ctx, claim)
	}
	r, err := s.Get(ctx, run.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range r.Items {
		item, err := s.Result(ctx, run.ID, entry.Index, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if item.Result == nil || item.Result.Outputs[0].Error != "" {
			t.Fatalf("candidate %d: %+v", entry.Index, item.Result)
		}
		if item.Result.Judgment.Status != "incomplete" {
			t.Fatal("failed judge claimed pass")
		}
		if !bytes.Equal(item.Result.Outputs[0].Request, p.Requests[entry.Index]) {
			t.Fatal("execution differs from production preview")
		}
	}
	blockers, err := s.PublicationBlockers(ctx, p.Candidates[0].Revision, time.Now())
	if err != nil || len(blockers) != 0 {
		t.Fatal("advisory judge blocks valid protocol", blockers, err)
	}
	var audience, env string
	if err = s.DB.QueryRow(`SELECT audience,environment FROM ai_cost_attempts LIMIT 1`).Scan(&audience, &env); err != nil || audience != "admin_evaluation" || env != "management" {
		t.Fatal("admin accounting", audience, env, err)
	}
	var holds int64
	s.DB.QueryRow(`SELECT SUM(remaining_nanos) FROM prompt_eval_budget_holds`).Scan(&holds)
	if holds != 0 {
		t.Fatal("completed hold leaked", holds)
	}
	next := m
	next.Key = uuid.NewString()
	if _, err = s.Start(ctx, p, next, time.Now()); err != nil {
		t.Fatal(err)
	}
	blockers, err = s.PublicationBlockers(ctx, p.Candidates[0].Revision, time.Now())
	if err != nil || len(blockers) == 0 {
		t.Fatal("stale pass accepted", err)
	}
}
