package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
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
		{`{"requestID":"` + id + `","purpose":"journal","consentVersion":"2026-10-20"}`, "", 401, "authentication_failed"},
		{`{"requestID":"` + id + `","purpose":"journal","consentVersion":""}`, "key", 403, "read_aloud_consent_required"},
		{`{"requestID":"` + id + `","purpose":"journal","consentVersion":"2026-10-20"}`, "key", 403, "managed_subscription_required"},
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
	db := testutil.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now().UTC()
	entitlements := mysqlstore.NewEntitlementRepository(db, "journal")
	for _, key := range []string{"valid-key", "second-device", "other-key"} {
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
	consents := privacy.NewService(mysqlstore.NewPrivacyRepository(db, "journal"), nil, nil, time.Now)
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
	access := &readaloud.Access{Entitlements: entitlements, Consents: consents, JournalCharacters: 3, PreviewCharacters: 1000, Now: time.Now}
	speech, err := readaloud.NewSessionService(ctx, readaloud.QuotaStore{DB: db}, metered, catalog, access.Authorize, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { speech.Run(); close(done) }()
	defer func() { speech.Close(); <-done }()
	accessFault := &speechAccessFault{ReadAloudAccess: access}
	accessFault.remaining.Store(-1)
	s := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: speechHTTPAuth{}, Privacy: consents, Consent: consents, ReadAloudSessions: speech, ReadAloudAccess: accessFault, ReadAloudCatalog: catalog})
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
		req.Header.Set("X-Tellyouwhat-Key-ID", key)
		req.Header.Set("X-Tellyouwhat-Assertion", "fixture")
		req.Header.Set("X-Tellyouwhat-Nonce", uuid.NewString())
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
		call("POST", "/v1/privacy/consents", key, map[string]any{"consents": []map[string]any{{"scope": privacy.JournalReadAloudScope, "documentVersion": privacy.JournalReadAloudDocumentVersion, "granted": value}}}, 200)
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
	req.Header.Set("X-Tellyouwhat-Key-ID", "valid-key")
	req.Header.Set("X-Tellyouwhat-Assertion", "fixture")
	req.Header.Set("X-Tellyouwhat-Nonce", uuid.NewString())
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
	subject, err := access.Subject(ctx, "valid-key")
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
}
