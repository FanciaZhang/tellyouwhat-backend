package development

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto/rand"
	"encoding/base64"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/gateway"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/journal/voicestore"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

type syntheticVoicePolicy struct{ revision promptconfig.Revision }

func (p syntheticVoicePolicy) Published(context.Context) (map[string]promptconfig.Revision, error) {
	return map[string]promptconfig.Revision{"journal": p.revision}, nil
}

// An opt-in, loopback-only candidate for synthetic App acceptance. It opens no
// production storage or Apple accounts and exits after its owner signals stop.
func TestConfiguredSyntheticVoiceAppServer(t *testing.T) {
	path := os.Getenv("JOURNAL_SYNTHETIC_VOICE_APP_CONFIG")
	if path == "" {
		t.Skip("explicit synthetic App integration only")
	}
	var c struct {
		BaseURL, APIKey, Lite, Pro, Voice, Token, OutputDirectory, ReplaySnapshot string
		ASR                                                                       voice.ASRConfig
		BudgetCNY                                                                 int64
		CompareNarrative, Durable                                                 bool
		ReplaySnapshots                                                           []string
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &c) != nil || c.APIKey == "" || len(c.Token) < 43 || c.Lite == "" || c.Pro == "" || c.Voice == "" || c.OutputDirectory == "" {
		t.Fatal("private candidate configuration required")
	}
	if c.BudgetCNY == 0 {
		c.BudgetCNY = 45
	}
	if c.BudgetCNY < 1 || c.BudgetCNY > 50 {
		t.Fatal("synthetic acceptance budget must be between 1 and 50 CNY")
	}
	if err := os.MkdirAll(c.OutputDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	c.ASR.ObserveTrace = func(traces []voice.StreamTrace) {
		data, err := json.MarshalIndent(map[string]any{"synthetic": true, "traces": traces}, "", "  ")
		if err == nil {
			err = os.WriteFile(filepath.Join(c.OutputDirectory, "asr-trace-"+uuid.NewString()+".json"), data, 0600)
		}
		if err != nil {
			t.Errorf("synthetic ASR evidence write failed: %T", err)
		}
	}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 10_000_000_000, OutputNanosPerMillionTokens: 100_000_000_000}
	policy := promptconfig.Defaults(c.Lite, c.Pro, c.Voice, 90)["journal"]
	policy.Journal.Voice.Parameters.Price = &price
	policy.Journal.Organize.Lite.Price = &price
	if c.Durable || c.CompareNarrative {
		policy.Journal.Voice.Parameters.MaxOutputTokens = 4096
		policy.Journal.Organize.Lite.MaxOutputTokens = 4096
	}
	published := time.Now()
	prompts := promptconfig.NewJournalCache(syntheticVoicePolicy{promptconfig.Revision{ID: "synthetic-voice-candidate", Scope: "journal", Policy: policy, PublishedAt: &published}})
	if err := prompts.Refresh(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	ledger, err := NewFileCostStore(filepath.Join(c.OutputDirectory, "cost-events.json"), c.BudgetCNY*costcontrol.NanosPerCNY, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	budget, err := costcontrol.New(ledger, costcontrol.Limits{MonthlyBudgetNanos: c.BudgetCNY * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: 15 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("synthetic", true)
	speech := voice.NewBudgetedSpeech(voice.ASR{Config: c.ASR, Prompts: prompts}, budget, "journal-synthetic-acceptance", costcontrol.DurationPrice{NanosPerHour: 4_500_000_000})
	configured := voice.ConfiguredRewriter{Next: voice.NewBudgetedRewriter(voice.ArkRewriter{BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.Voice, Logger: logger}, budget, "journal-synthetic-acceptance", price), Config: prompts}
	rewriter := syntheticVoiceTraceRewriter{Next: configured, Directory: c.OutputDirectory}
	if c.CompareNarrative {
		compareDurableNarrative(t, rewriter, prompts, c.OutputDirectory)
	}
	if c.ReplaySnapshot != "" {
		if filepath.Dir(c.ReplaySnapshot) != filepath.Dir(c.OutputDirectory) {
			t.Fatal("replay must belong to this synthetic run")
		}
		raw, err := os.ReadFile(c.ReplaySnapshot)
		var snapshot voice.Snapshot
		if err != nil || json.Unmarshal(raw, &snapshot) != nil {
			t.Fatal("synthetic replay unavailable")
		}
		result, callErr := rewriter.Rewrite(t.Context(), snapshot, 1)
		t.Logf("synthetic replay completed success=%t stage=%s", callErr == nil, result.Diagnostics.Stage)
	}
	for _, path := range c.ReplaySnapshots {
		if filepath.Dir(path) != c.OutputDirectory {
			t.Fatal("replay must belong to this synthetic run")
		}
		raw, err := os.ReadFile(path)
		var trace struct{ Snapshot voice.Snapshot }
		if err != nil || json.Unmarshal(raw, &trace) != nil {
			t.Fatal("synthetic replay unavailable")
		}
		trace.Snapshot.Organization = true
		started := time.Now()
		result, err := rewriter.Rewrite(t.Context(), trace.Snapshot, 1)
		record := map[string]any{"synthetic": true, "route": "compactPro", "sourceTrace": filepath.Base(path), "elapsedMilliseconds": time.Since(started).Milliseconds(), "model": result.Model, "polish": result.Polish, "inputTokens": result.InputTokens, "outputTokens": result.OutputTokens, "passed": err == nil}
		raw, _ = json.MarshalIndent(record, "", "  ")
		if os.WriteFile(filepath.Join(c.OutputDirectory, "compact-pro-"+filepath.Base(path)), raw, 0600) != nil {
			t.Fatal("replay evidence unavailable")
		}
		if err != nil {
			t.Fatal("compact Pro replay failed", result.Diagnostics.Stage)
		}
	}
	secret := sha256.Sum256([]byte(c.Token))
	service := &voice.Service{Store: voice.NewMemoryStore(), Speech: speech, Model: rewriter, Secret: secret[:], Limit: 120 * 60 * 1000, Logger: logger}
	var stores []voicestore.Store
	if c.Durable {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(key))
		if err != nil {
			t.Fatal(err)
		}
		stores = []voicestore.Store{{Database: testutil.MySQL(t), Cipher: cipher}}
	}
	handler := syntheticVoiceAppHandler(t, c.Token, prompts, service, time.Now, stores...)
	server := httptest.NewServer(handler)
	defer server.Close()
	ready, _ := json.Marshal(map[string]any{"url": server.URL, "voiceProtocol": voice.Version, "budgetCNY": c.BudgetCNY, "synthetic": true})
	if err := os.WriteFile(filepath.Join(c.OutputDirectory, "ready.json"), ready, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(75 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.Context().Done():
			t.Fatal("candidate interrupted")
		case <-deadline.C:
			t.Fatal("candidate acceptance deadline exceeded")
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(c.OutputDirectory, "stop")); err == nil {
				return
			}
		}
	}
}

// This is an expiring operator grant for paid synthetic provider checks, not a
// simulated Apple purchase. The normal purchase checker remains unchanged.
func syntheticVoiceAppHandler(t *testing.T, token string, prompts *promptconfig.Cache, service *voice.Service, now func() time.Time, stores ...voicestore.Store) http.Handler {
	digest := sha256.Sum256([]byte(token))
	keyID, deviceID := uuid.NewString(), uuid.NewString()
	keys := attestation.NewMemoryKeyStore()
	keys.Put(attestation.RegisteredKey{AppID: "journal", KeyID: keyID, DeviceID: deviceID, Environment: "development"})
	started := now()
	grants := []entitlement.DevelopmentGrant{{KeyID: keyID, DeviceID: deviceID, StartedAt: started, ExpiresAt: started.Add(75 * time.Minute)}}
	access := &entitlement.DevelopmentAccess{AppID: "journal", Purchases: entitlement.NewMemoryStore(), Keys: keys,
		Grants: func() ([]entitlement.DevelopmentGrant, error) { return grants, nil }, Now: now}
	consent := privacy.NewService(privacy.NewMemoryRepository(), nil, nil, now)
	guard := func(g *gin.Context) {
		r := g.Request
		if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
			g.AbortWithStatusJSON(200, map[string]any{"status": "ready", "environment": "development"})
			return
		}
		stream := r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/journal/voice/sessions/") && strings.HasSuffix(r.URL.Path, "/stream")
		organization := strings.HasPrefix(r.URL.Path, "/v1/journal/voice/organizations") && (r.Method == http.MethodPost || r.Method == http.MethodGet)
		allowed := organization || stream || r.Method == http.MethodPost && (r.URL.Path == "/v1/privacy/consents" || r.URL.Path == "/v1/journal/voice/sessions")
		if !allowed {
			g.AbortWithStatus(404)
			return
		}
		if !stream {
			provided := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(digest[:], provided[:]) != 1 {
				g.AbortWithStatus(401)
				return
			}
			principal := attestation.Principal{AppID: "journal", KeyID: keyID, DeviceID: deviceID}
			g.Request = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		}
		g.Next()
	}
	var organizations *voice.OrganizationService
	if len(stores) == 1 {
		registered := attestation.RegisteredKey{KeyID: keyID, DeviceID: deviceID, Environment: "development", PublicKey: []byte("synthetic"), Receipt: []byte("synthetic")}
		if err := mysqlstore.NewKeyRepository(stores[0].Database, "journal").Register(t.Context(), registered); err != nil {
			t.Fatal(err)
		}
		principal := attestation.Principal{AppID: "journal", KeyID: keyID, DeviceID: deviceID}
		organizations = &voice.OrganizationService{Store: stores[0], Model: service.Model, Prompts: prompts, Now: now,
			Authorize: func(ctx context.Context, owner string) bool {
				granted, err := access.HasManagedSubscription(ctx, principal)
				approved, consentErr := consent.HasRequiredConsents(ctx, principal, []string{privacy.ManagedAIScope})
				return owner == keyID && err == nil && granted && consentErr == nil && approved
			}, Admit: func(context.Context, voice.OrganizationJob, voice.Snapshot) (func(voice.RewriteResult, error), error) {
				return func(voice.RewriteResult, error) {}, nil
			}}
		workerContext, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); organizations.Run(workerContext) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	server := gateway.New(gateway.Dependencies{JournalVoiceOrganizations: organizations, HTTPMiddleware: []gin.HandlerFunc{guard}, App: appregistry.App{ID: appregistry.Journal, AllowedOperationPrefix: "journal."},
		Authenticator: authenticator{}, Entitlements: access, Privacy: consent, Consent: consent,
		RequiredConsentScopes: []string{privacy.ManagedAIScope}, AllowedConsentScopes: []string{privacy.ManagedAIScope, privacy.JournalIllustrationScope, privacy.JournalReadAloudScope},
		PromptConfig: prompts, Voice: service, VoiceEntitlements: access, Now: now})
	router := server.Router()
	router.ContextWithFallback = true
	return router
}

func TestSyntheticVoiceCandidateRequiresExpiringGrantAndConsent(t *testing.T) {
	now := time.Now()
	published := now
	prompts := promptconfig.NewJournalCache(syntheticVoicePolicy{promptconfig.Revision{ID: "synthetic", Scope: "journal", PublishedAt: &published, Policy: promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]}})
	if err := prompts.Refresh(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("s", 43)
	service := &voice.Service{Store: voice.NewMemoryStore(), Speech: voice.ASR{}, Model: voice.ArkRewriter{}, Secret: make([]byte, 32), Limit: 120 * 60 * 1000}
	handler := syntheticVoiceAppHandler(t, token, prompts, service, func() time.Time { return now })
	request := func(path, body, credential string) int {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Tellyouwhat-Request-ID", uuid.NewString())
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	body := `{"sessionID":"` + uuid.NewString() + `","consentVersion":"` + voice.Version + `"}`
	if request("/v1/journal/voice/sessions", body, "") != 401 || request("/v1/journal/voice/sessions", body, token) != 403 {
		t.Fatal("candidate accepted missing authorization or consent")
	}
	consents := `{"consents":[{"scope":"managed_subscription","documentVersion":"2026-11-03","granted":true},{"scope":"journal_illustration","documentVersion":"2026-11-03","granted":true},{"scope":"journal_read_aloud","documentVersion":"2026-11-03","granted":true}]}`
	if request("/v1/privacy/consents", consents, token) != 200 || request("/v1/journal/voice/sessions", body, token) != 201 {
		t.Fatal("explicit candidate grant did not authorize the consented session")
	}
	now = now.Add(76 * time.Minute)
	if request("/v1/journal/voice/sessions", body, token) != 403 {
		t.Fatal("candidate grant did not expire")
	}
}

type syntheticVoiceTraceRewriter struct {
	Next      voice.Rewriter
	Directory string
}

func (r syntheticVoiceTraceRewriter) Rewrite(ctx context.Context, snapshot voice.Snapshot, revision int) (voice.RewriteResult, error) {
	result, err := r.Next.Rewrite(ctx, snapshot, revision)
	record := map[string]any{"synthetic": true, "snapshot": snapshot, "outputText": result.OutputText, "diagnostics": result.Diagnostics, "model": result.Model, "durableCompact": snapshot.Organization, "succeeded": err == nil, "inputTokens": result.InputTokens, "outputTokens": result.OutputTokens}
	data, encodeErr := json.MarshalIndent(record, "", "  ")
	if encodeErr == nil {
		encodeErr = os.WriteFile(filepath.Join(r.Directory, "model-"+uuid.NewString()+".json"), data, 0600)
	}
	if encodeErr != nil {
		return result, encodeErr
	}
	return result, err
}

func compareDurableNarrative(t *testing.T, model voice.Rewriter, prompts *promptconfig.Cache, directory string) {
	t.Helper()
	cases := [][]string{
		{"今天下午3点，我和小林在青禾咖啡馆见面。原本说预算300元，后来改成280元。", "钱还没付，不是已经付款。我可能周六出发，还没有决定。"},
		{"今天上午我去了图书馆。嗯，路上没下雨。", "回来后我给阳台上的薄荷浇水，如果明天下雨，就留在家整理书架。", "我还没想清楚要不要送掉旧书，先留着吧。"},
	}
	for index, texts := range cases {
		author := uuid.NewString()
		s := voice.Snapshot{DictationMode: true, WritingStyle: "natural", Narrator: &voice.Narrator{PersonID: author, Name: "我"}}
		s.Polish = &voice.PolishRequest{Narrator: s.Narrator}
		for _, text := range texts {
			turn := voice.SourceUtterance{ID: uuid.NewString(), Text: text, PersonID: author, Person: "我"}
			s.Polish.Targets = append(s.Polish.Targets, voice.PolishTarget{ID: uuid.NewString(), Text: text, SourceText: text, SourceIDs: []string{turn.ID}, Turns: []voice.SourceUtterance{turn}, Style: "body", CompleteSource: true})
		}
		for _, compact := range []bool{true, false} {
			s.Organization = compact
			ctx, err := promptconfig.Freeze(t.Context(), prompts)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := voice.PrepareRewrite(ctx, s, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			result, callErr := model.Rewrite(ctx, s, 1)
			record := map[string]any{"synthetic": true, "sample": index, "compact": compact, "requestBytes": len(prepared.Body), "elapsedMilliseconds": time.Since(started).Milliseconds(), "model": result.Model, "inputTokens": result.InputTokens, "outputTokens": result.OutputTokens, "polish": result.Polish, "passed": callErr == nil && result.Polish != nil}
			data, _ := json.MarshalIndent(record, "", "  ")
			name := filepath.Join(directory, fmt.Sprintf("comparison-%d-%t.json", index, compact))
			if err := os.WriteFile(name, data, 0600); err != nil {
				t.Fatal(err)
			}
			if callErr != nil {
				t.Fatalf("comparison failed: stage=%s", result.Diagnostics.Stage)
			}
		}
	}
}
