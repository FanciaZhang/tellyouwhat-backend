package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
)

type imageStoreFixture struct {
	owner string
	saved bool
	job   illustration.Job
}

func (f *imageStoreFixture) Create(_ context.Context, j illustration.Job) (illustration.Job, error) {
	f.owner = j.Task.OwnerID
	f.job = j
	f.saved = true
	return j, nil
}
func (f *imageStoreFixture) Get(_ context.Context, owner, _ string) (illustration.Job, error) {
	f.owner = owner
	if owner != f.job.Task.OwnerID {
		return illustration.Job{}, illustration.ErrNotFound
	}
	return f.job, nil
}
func (f *imageStoreFixture) Save(_ context.Context, j illustration.Job, _ uint64) error {
	f.saved = true
	f.job = j
	return nil
}
func (*imageStoreFixture) Pending(context.Context, time.Time, int) ([]illustration.Task, error) {
	return nil, nil
}

type imageGeneratorFixture struct{}

func (imageGeneratorFixture) Model() string { return "image-model" }
func (imageGeneratorFixture) Generate(context.Context, illustration.Input) (illustration.Result, error) {
	return illustration.Result{}, errors.New("unexpected dispatch")
}

func TestSharedJournalImageAdmissionBindsOwnerAndExplicitConsent(t *testing.T) {
	for _, tc := range []struct {
		name, app                    string
		authenticated, paid, consent bool
		status                       int
	}{
		{"unauthenticated", "journal", false, true, true, 401},
		{"wrong app", "health", true, true, true, 401},
		{"unpaid", "journal", true, false, true, 403},
		{"no consent", "journal", true, true, false, 403},
		{"authorized", "journal", true, true, true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &imageStoreFixture{}
			service := &illustration.Service{Store: store, Generator: imageGeneratorFixture{}, Now: time.Now}
			server := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: tc.app}, Entitlements: fakeEntitlements{allowed: tc.paid}, Consent: fakeConsentGate{granted: tc.consent}, JournalIllustrations: service})
			body := `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c","versionID":"8d43cd74-5652-4412-b097-303f563e673a","brief":{"title":"river","details":[{"kind":"other","text":"river"}]},"consentRevision":"` + privacy.JournalIllustrationDocumentVersion + `"}`
			r := authorizedRequest(http.MethodPost, "/v1/journal/illustrations", body)
			if !tc.authenticated {
				r.Header.Del("X-Tellyouwhat-Assertion")
			}
			w := httptest.NewRecorder()
			server.Router().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if store.saved != (tc.status == 200) {
				t.Fatal("denied request persisted or valid request lost")
			}
			if tc.status == 200 && (store.owner != "valid-key" || store.job.Input == nil || !strings.Contains(store.job.Input.Prompt, "river")) {
				t.Fatal("request owner or frozen prompt incorrect", store.owner)
			}
		})
	}
}

func TestSharedJournalImageCancellationSurvivesExpiredEntitlement(t *testing.T) {
	now := time.Now()
	taskID := "8d43cd74-5652-4412-b097-303f563e673a"
	store := &imageStoreFixture{job: illustration.Job{Task: illustration.Task{ID: taskID, OwnerID: "valid-key", State: illustration.TaskRunning, Revision: 2, Charge: illustration.ChargeUncertain, ExpiresAt: now.Add(time.Hour)}, Input: &illustration.Input{Prompt: "private"}}}
	server := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: "journal"}, Entitlements: fakeEntitlements{allowed: false}, Consent: fakeConsentGate{granted: false}, JournalIllustrations: &illustration.Service{Store: store, Now: time.Now}})
	r := authorizedRequest(http.MethodPost, "/v1/journal/illustrations/"+taskID+"/cancel", `{"revision":2}`)
	w := httptest.NewRecorder()
	server.Router().ServeHTTP(w, r)
	if w.Code != 200 || !store.saved || store.job.Task.State != illustration.TaskCancelled || store.job.Input != nil {
		t.Fatalf("revocation blocked: %d %s", w.Code, w.Body.String())
	}
	r = authorizedRequest(http.MethodGet, "/v1/journal/illustrations/"+taskID+"/result", "")
	w = httptest.NewRecorder()
	server.Router().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("expired access exposed image")
	}
}

type blockedImageBody struct {
	entered, release chan struct{}
	once             sync.Once
}

func (b *blockedImageBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return 0, io.EOF
}
func (*blockedImageBody) Close() error { return nil }
func TestSharedJournalImageAdmissionBoundsBodyAllocation(t *testing.T) {
	store := &imageStoreFixture{}
	server := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: "journal"}, JournalIllustrations: &illustration.Service{Store: store, Now: time.Now}})
	body := &blockedImageBody{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(body.release) })
	first := authorizedRequest(http.MethodPost, "/v1/journal/illustrations", "")
	first.Body = body
	done := make(chan struct{})
	go func() { defer close(done); server.Router().ServeHTTP(httptest.NewRecorder(), first) }()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter body reader")
	}
	second := authorizedRequest(http.MethodPost, "/v1/journal/illustrations", "")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, second)
	if response.Code != 429 || response.Header().Get("Retry-After") == "" {
		t.Fatal("concurrent large body admitted")
	}
	release.Do(func() { close(body.release) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("body slot not released")
	}
	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/journal/illustrations", ""))
	if response.Code == 429 {
		t.Fatal("body slot leaked after invalid request")
	}
}
