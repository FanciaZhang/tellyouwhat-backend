package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
	"github.com/tellyouwhat/backend/internal/journal/simulatortest"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

// Fixtures exercise real HTTP, privacy, catalog and SQL accounting. They do not
// claim Apple attestation or actual Doubao audio acceptance.
type speechHTTPAuth struct{}

func (speechHTTPAuth) Authenticate(_ context.Context, p RequestProof) (Principal, error) {
	if p.KeyID == "" || p.Assertion != "fixture" || uuid.Validate(p.Nonce) != nil || uuid.Validate(p.RequestID) != nil {
		return Principal{}, attestation.ErrAuthentication
	}
	return Principal{AppID: "journal", KeyID: p.KeyID, DeviceID: "fixture-device"}, nil
}

type deniedSpeechAccess struct{}

func (deniedSpeechAccess) Grant(context.Context, string, string) (readaloud.Grant, error) {
	return readaloud.Grant{}, readaloud.ErrSubscriptionRequired
}
func (deniedSpeechAccess) Subject(context.Context, string) (readaloud.Subject, error) {
	return readaloud.Subject{}, readaloud.ErrSubscriptionRequired
}
func (deniedSpeechAccess) Consent(context.Context, string, string) error {
	return readaloud.ErrConsentRequired
}
func TestReadAloudHTTPAdmissionChecksActualRouterBodyAuthAndConsent(t *testing.T) {
	s := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: speechHTTPAuth{}, ReadAloudSessions: &readaloud.SessionService{}, ReadAloudAccess: deniedSpeechAccess{}})
	id := uuid.NewString()
	for _, test := range []struct {
		body, key string
		status    int
		code      string
	}{
		{strings.Repeat("x", 8193), "", 413, "payload_too_large"},
		{`{"requestID":"` + id + `","purpose":"journal","consentVersion":"2026-11-03"}`, "", 401, "authentication_failed"},
		{`{"requestID":"` + id + `","purpose":"journal","consentVersion":""}`, "key", 403, "read_aloud_consent_required"},
		{`{"requestID":"` + id + `","purpose":"journal","consentVersion":"2026-11-03"}`, "key", 403, "managed_subscription_required"},
	} {
		req := httptest.NewRequest("POST", "/v1/journal/read-aloud/syntheses", strings.NewReader(test.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tellyouwhat-Request-ID", id)
		req.Header.Set("X-Tellyouwhat-Key-ID", test.key)
		req.Header.Set("X-Tellyouwhat-Assertion", "fixture")
		req.Header.Set("X-Tellyouwhat-Nonce", uuid.NewString())
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, req)
		if w.Code != test.status || !strings.Contains(w.Body.String(), test.code) {
			t.Fatal("wrong real route admission", w.Code, w.Body.String())
		}
	}
}

type speechHTTPProvider struct {
	calls   atomic.Int32
	block   atomic.Bool
	started chan string
}
type speechFlushRecorder struct {
	*httptest.ResponseRecorder
	once    sync.Once
	flushed chan struct{}
}
type speechAccessFault struct {
	ReadAloudAccess
	remaining atomic.Int32
}

func (a *speechAccessFault) Consent(ctx context.Context, key, purpose string) error {
	for {
		n := a.remaining.Load()
		if n < 0 {
			break
		}
		if n == 0 {
			return readaloud.ErrAccessUnavailable
		}
		if a.remaining.CompareAndSwap(n, n-1) {
			break
		}
	}
	return a.ReadAloudAccess.Consent(ctx, key, purpose)
}

func (w *speechFlushRecorder) Flush() {
	w.ResponseRecorder.Flush()
	w.once.Do(func() { close(w.flushed) })
}

func (p *speechHTTPProvider) Synthesize(ctx context.Context, in readaloud.Synthesis, emit func([]byte) error) (readaloud.SynthesisUsage, error) {
	p.calls.Add(1)
	if err := emit([]byte{1, 2}); err != nil {
		return readaloud.SynthesisUsage{}, err
	}
	p.started <- in.RequestID
	if p.block.Load() {
		<-ctx.Done()
		return readaloud.SynthesisUsage{}, ctx.Err()
	}
	if err := emit([]byte{3, 4}); err != nil {
		return readaloud.SynthesisUsage{}, err
	}
	return readaloud.SynthesisUsage{Characters: utf8.RuneCountInString(in.Text), UsageKnown: true, AudioBytes: 4}, nil
}
func TestMySQLReadAloudHTTPStreamingResumptionConsentAndSubscriptionQuota(t *testing.T) {
	runReadAloudHTTPAccounting(t, false)
}

func TestMySQLSignedSimulatorReadAloudHTTPAccountingAndConsent(t *testing.T) {
	runReadAloudHTTPAccounting(t, true)
}

func runReadAloudHTTPAccounting(t *testing.T, simulatorIdentity bool) {
	t.Helper()
	db := testutil.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now().UTC()
	app := appregistry.App{ID: appregistry.Journal}
	var simulator *simulatortest.Service
	aliases := map[string]string{}
	privateKeys := map[string]ed25519.PrivateKey{}
	nonces := attestation.NewMemoryNonceStore()
	if simulatorIdentity {
		app.BundleID = simulatortest.BundleID
		app.Hosts = []string{simulatortest.Host}
		registry := simulatortest.Registry{Version: 1}
		sharedSubject := uuid.NewString()
		start, end := now.Add(-time.Hour), now.Add(24*time.Hour)
		for _, name := range []string{"valid-key", "second-device", "other-key"} {
			public, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			key := "simulator_" + uuid.NewString()
			aliases[name] = key
			privateKeys[key] = private
			subject := sharedSubject
			if name == "other-key" {
				subject = uuid.NewString()
			}
			registry.Credentials = append(registry.Credentials, simulatortest.Credential{KeyID: key, PublicKey: base64.StdEncoding.EncodeToString(public), SubjectID: subject, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), SubscriptionStartedAt: &start, SubscriptionExpiresAt: &end, JournalCharacters: 3, PreviewCharacters: 1000})
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "registry.json")
		body, err := json.Marshal(registry)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		simulator, err = simulatortest.New(path, "development", app, nonces, time.Now)
		if err != nil {
			t.Fatal(err)
		}
	}
	keyFor := func(name string) string {
		if key, ok := aliases[name]; ok {
			return key
		}
		return name
	}
	entitlements := mysqlstore.NewEntitlementRepository(db, "journal")
	for _, key := range []string{"valid-key", "second-device", "other-key"} {
		if simulatorIdentity {
			continue
		}
		if err := mysqlstore.NewKeyRepository(db, "journal").Register(ctx, attestation.RegisteredKey{KeyID: key, DeviceID: key, Environment: "production", PublicKey: []byte("synthetic"), Receipt: []byte("synthetic")}); err != nil {
			t.Fatal(err)
		}
		purchase := "same-purchase"
		if key == "other-key" {
			purchase = "another-purchase"
		}
		if err := entitlements.UpsertVerified(ctx, entitlement.Record{KeyID: key, TransactionID: purchase, Environment: "sandbox", StartedAt: now.Add(-time.Hour), ExpiresAt: now.Add(24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	var consentRepository privacy.Repository = mysqlstore.NewPrivacyRepository(db, "journal")
	if simulatorIdentity {
		repository := simulatortest.PrivacyRepository{Repository: consentRepository, DB: db, Simulator: simulator}
		if err := repository.Ready(ctx); err != nil {
			t.Fatal(err)
		}
		consentRepository = repository
	}
	consents := privacy.NewService(consentRepository, nil, nil, time.Now)
	actor := uuid.NewString()
	if _, err := db.ExecContext(ctx, "INSERT INTO admin_users(id,webauthn_id,display_name,role,status,created_at,updated_at) VALUES(?,UNHEX(REPLACE(UUID(),'-','')),'Speech HTTP test','admin','active',?,?)", actor, now, now); err != nil {
		t.Fatal(err)
	}
	mutation := func() aiconfig.Mutation {
		return aiconfig.Mutation{Actor: actor, Key: uuid.NewString(), RequestID: uuid.NewString()}
	}
	v := readaloud.Voice{ID: "synthetic_voice", Name: "测试声音", Languages: []string{"zh-cn"}, Styles: []string{"natural"}, ResourceID: readaloud.ResourceID, BillingClass: "standard", ProbeText: "专门编写的公开试听短句。", Enabled: true}
	v, _, err := readaloud.VerifyVoice(ctx, v, catalogSpeechFixture{}, now)
	if err != nil {
		t.Fatal(err)
	}
	catalog := readaloud.CatalogStore{DB: db}
	probe := mutation()
	if _, err := catalog.ClaimProbe(ctx, probe, v, now); err != nil {
		t.Fatal(err)
	}
	if err := catalog.FinishProbe(ctx, probe, v, true, now); err != nil {
		t.Fatal(err)
	}
	draft, err := catalog.Draft(ctx, readaloud.CatalogDraft{DefaultVoiceID: v.ID, Voices: []readaloud.VoiceConfiguration{readaloud.Configuration(v)}}, mutation(), now)
	if err != nil {
		t.Fatal(err)
	}
	published, err := catalog.Publish(ctx, readaloud.CatalogPublication{Revision: draft.Version}, mutation(), now)
	if err != nil {
		t.Fatal(err)
	}
	public, err := published.Public()
	if err != nil {
		t.Fatal(err)
	}
	provider := &speechHTTPProvider{started: make(chan string, 8)}
	costs, err := costcontrol.New(mysqlstore.NewCostControlStore(db), costcontrol.Limits{MonthlyBudgetNanos: 10 * costcontrol.NanosPerCNY, MaxConcurrent: 2, LeaseDuration: time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	metered, err := readaloud.NewMeteredSynthesizer(provider, costs, readaloud.StandardCharacterNanos)
	if err != nil {
		t.Fatal(err)
	}
	normalAccess := &readaloud.Access{Entitlements: entitlements, Consents: consents, JournalCharacters: 3, PreviewCharacters: 1000, Now: time.Now}
	var access ReadAloudAccess = normalAccess
	authorize := normalAccess.Authorize
	if simulatorIdentity {
		testAccess := simulatortest.Access{Simulator: simulator, Normal: normalAccess}
		access, authorize = testAccess, testAccess.Authorize
	}
	speech, err := readaloud.NewSessionService(ctx, readaloud.QuotaStore{DB: db}, metered, catalog, authorize, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { speech.Run(); close(done) }()
	defer func() { speech.Close(); <-done }()
	accessFault := &speechAccessFault{ReadAloudAccess: access}
	accessFault.remaining.Store(-1)
	dependencies := Dependencies{App: app, Authenticator: speechHTTPAuth{}, Privacy: consents, Consent: consents, ReadAloudSessions: speech, ReadAloudAccess: accessFault, ReadAloudCatalog: catalog}
	if simulator != nil {
		dependencies.ReadAloudSimulatorAuthenticator = simulator
	}
	s := New(dependencies)
	sign := func(req *http.Request, name string) {
		t.Helper()
		key := keyFor(name)
		req.Header.Set("X-Tellyouwhat-Key-ID", key)
		req.Header.Set("X-Tellyouwhat-Assertion", "fixture")
		req.Header.Set("X-Tellyouwhat-Nonce", uuid.NewString())
		if !simulatorIdentity {
			return
		}
		req.Host = simulatortest.Host
		req.Header.Set("X-Tellyouwhat-Authentication", simulatortest.Scheme)
		if key == "" {
			return
		}
		nonce, err := nonces.Issue(ctx, key, 5*time.Minute, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var body []byte
		if req.Body != nil {
			var err error
			body, err = io.ReadAll(req.Body)
			req.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		proof := RequestProof{Method: req.Method, Path: req.URL.RequestURI(), RequestID: req.Header.Get("X-Tellyouwhat-Request-ID"), KeyID: key, Nonce: nonce, Timestamp: time.Now().UTC().Format(time.RFC3339), BodySHA256: contracts.BodySHA256(body)}
		req.Header.Set("X-Tellyouwhat-Nonce", nonce)
		req.Header.Set("X-Tellyouwhat-Timestamp", proof.Timestamp)
		req.Header.Set("X-Tellyouwhat-Assertion", base64.StdEncoding.EncodeToString(ed25519.Sign(privateKeys[key], simulatortest.Message(proof))))
	}
	call := func(method, path, key string, body any, want int) *httptest.ResponseRecorder {
		t.Helper()
		raw := ""
		id := uuid.NewString()
		if body != nil {
			data, e := json.Marshal(body)
			if e != nil {
				t.Fatal(e)
			}
			raw = string(data)
			if request, ok := body.(journalhttpapi.ReadAloudSynthesisRequest); ok {
				id = request.RequestID.String()
			}
		}
		req := httptest.NewRequest(method, path, strings.NewReader(raw))
		req = req.WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tellyouwhat-Request-ID", id)
		sign(req, key)
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s got %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	const base = "/v1/journal/read-aloud"
	request := journalhttpapi.ReadAloudSynthesisRequest{RequestID: uuid.New(), VoiceID: v.ID, VoiceRevision: public.Voices[0].Revision, Purpose: "journal", Text: "测试", Style: "natural", Language: "zh-cn", ConsentVersion: privacy.JournalReadAloudDocumentVersion}
	call("POST", base+"/syntheses", "", request, 401)
	call("POST", base+"/syntheses", "valid-key", request, 403)
	grantConsent := func(key string, value bool) {
		call("POST", "/v1/privacy/consents", key, map[string]any{"consents": privacy.JournalCloudAIConsents(value)}, 200)
	}
	grantConsent("valid-key", true)
	call("POST", base+"/syntheses", "valid-key", request, 200)
	streamPath := base + "/syntheses/" + request.RequestID.String() + "/events"
	stream := call("GET", streamPath, "valid-key", nil, 200)
	if stream.Header().Get("Content-Type") != "text/event-stream" || stream.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("unsafe stream headers")
	}
	decode := func(text string) ([]byte, int) {
		t.Helper()
		pcm := []byte{}
		terminal := 0
		for _, block := range strings.Split(text, "\n\n") {
			for _, line := range strings.Split(block, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event journalhttpapi.ReadAloudEvent
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				if event.Pcm != nil {
					pcm = append(pcm, (*event.Pcm)...)
				}
				if event.Kind == "state" {
					terminal++
					if event.Session == nil || event.Session.State != "succeeded" || event.Session.AudioBytes != 4 {
						t.Fatal("missing successful terminal", event)
					}
				}
			}
		}
		return pcm, terminal
	}
	pcm, terminal := decode(stream.Body.String())
	if string(pcm) != string([]byte{1, 2, 3, 4}) || terminal != 1 {
		t.Fatal("incomplete stream", pcm, terminal)
	}
	resumed, terminal := decode(call("GET", streamPath+"?after=2", "valid-key", nil, 200).Body.String())
	if string(resumed) != string([]byte{3, 4}) || terminal != 1 {
		t.Fatal("resume duplicated PCM", resumed)
	}
	call("POST", base+"/syntheses", "valid-key", request, 200)
	if provider.calls.Load() != 1 {
		t.Fatal("HTTP replay repeated purchase")
	}
	for range cap(s.readAloudStreams) {
		s.readAloudStreams <- struct{}{}
	}
	call("GET", streamPath, "valid-key", nil, 429)
	for range cap(s.readAloudStreams) {
		<-s.readAloudStreams
	}
	call("GET", base+"/syntheses/"+request.RequestID.String(), "other-key", nil, 404)
	grantConsent("second-device", true)
	second := request
	second.RequestID = uuid.New()
	call("POST", base+"/syntheses", "second-device", second, 429)
	if provider.calls.Load() != 1 {
		t.Fatal("second device reset subscription allowance")
	}
	var allowance journalhttpapi.ReadAloudAllowance
	w := call("GET", base+"/allowance", "second-device", nil, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &allowance); err != nil || allowance.Used != 2 || allowance.Remaining != 1 {
		t.Fatal("incorrect shared allowance", allowance, err)
	}
	<-provider.started
	provider.block.Store(true)
	preview := request
	preview.RequestID = uuid.New()
	preview.Purpose = "preview"
	preview.Text = ""
	preview.ConsentVersion = ""
	call("POST", base+"/syntheses", "valid-key", preview, 200)
	select {
	case <-provider.started:
	case <-ctx.Done():
		t.Fatal("preview did not start")
	}
	accessFault.remaining.Store(1)
	fault := call("GET", base+"/syntheses/"+preview.RequestID.String()+"/events", "valid-key", nil, 200)
	if !strings.Contains(fault.Body.String(), "authorization_unavailable") {
		t.Fatal("authorization outage was hidden", fault.Body.String())
	}
	accessFault.remaining.Store(-1)
	streamCtx, stopStream := context.WithCancel(ctx)
	req := httptest.NewRequest("GET", base+"/syntheses/"+preview.RequestID.String()+"/events", nil).WithContext(streamCtx)
	req.Header.Set("X-Tellyouwhat-Request-ID", uuid.NewString())
	sign(req, "valid-key")
	recorder := &speechFlushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	streamDone := make(chan struct{})
	go func() { s.Router().ServeHTTP(recorder, req); close(streamDone) }()
	select {
	case <-recorder.flushed:
	case <-ctx.Done():
		t.Fatal("stream headers did not flush")
	}
	stopStream()
	select {
	case <-streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE ignored the actual request cancellation")
	}
	subject, err := access.Subject(ctx, keyFor("valid-key"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := speech.Get(ctx, subject, preview.RequestID.String()); err != nil || !info.BufferAvailable || provider.calls.Load() != 2 {
		t.Fatal("connection loss cancelled or repeated the original paid job", info, err)
	}
	grantConsent("valid-key", false)
	call("GET", streamPath, "valid-key", nil, 403)
	speech.Close()
	<-done
	grantConsent("valid-key", true)
	call("GET", streamPath, "valid-key", nil, 410)
	call("DELETE", base+"/syntheses/"+request.RequestID.String(), "valid-key", nil, 200)
	if provider.calls.Load() != 2 {
		t.Fatal("withdrawal or recovery bought another synthesis")
	}
	if simulatorIdentity {
		for _, table := range []string{"app_attest_keys", "managed_entitlements", "privacy_consents"} {
			var count int
			if err := db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE key_id LIKE 'simulator_%'").Scan(&count); err != nil || count != 0 {
				t.Fatal("simulator polluted Apple identity or purchases", table, count, err)
			}
		}
	}
}
