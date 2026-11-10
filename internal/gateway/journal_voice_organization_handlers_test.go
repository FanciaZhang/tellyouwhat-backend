package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

type organizationTestRewriter struct{}

func (organizationTestRewriter) Rewrite(context.Context, voice.Snapshot, int) (voice.RewriteResult, error) {
	return voice.RewriteResult{}, voice.ErrInvalid
}

type organizationStoreFixture struct {
	job   voice.OrganizationJob
	saved bool
}

func (f *organizationStoreFixture) Create(_ context.Context, j voice.OrganizationJob) (voice.OrganizationJob, error) {
	f.saved = true
	f.job = j
	return j, nil
}
func (f *organizationStoreFixture) Get(_ context.Context, owner, id string) (voice.OrganizationJob, error) {
	if owner != f.job.OwnerID || id != f.job.ID {
		return voice.OrganizationJob{}, voice.ErrOrganizationNotFound
	}
	return f.job, nil
}
func (f *organizationStoreFixture) Save(_ context.Context, j voice.OrganizationJob, previous uint64) error {
	if f.job.Revision != previous {
		return voice.ErrOrganizationConflict
	}
	f.job = j
	f.saved = true
	return nil
}
func (*organizationStoreFixture) Pending(context.Context, time.Time, int) ([][2]string, error) {
	return nil, nil
}
func (f *organizationStoreFixture) ForgetOwner(context.Context, string) error {
	f.job = voice.OrganizationJob{}
	return nil
}

func TestJournalVoiceOrganizationAdmissionAndOwnerBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, app                    string
		authenticated, paid, consent bool
		status                       int
	}{
		{"unauthenticated", "journal", false, true, true, 401}, {"wrong app", "health", true, true, true, 401},
		{"unpaid", "journal", true, false, true, 403}, {"no consent", "journal", true, true, false, 403}, {"authorized", "journal", true, true, true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &organizationStoreFixture{}
			service := &voice.OrganizationService{Store: store, Model: organizationTestRewriter{}, Now: time.Now, Authorize: func(context.Context, string) bool { return true }}
			server := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: tc.app}, Entitlements: fakeEntitlements{allowed: tc.paid}, Consent: fakeConsentGate{granted: tc.consent}, JournalVoiceOrganizations: service})
			id := "19be2f9e-bd92-4699-b561-e3816092114c"
			author := uuid.NewString()
			source := uuid.NewString()
			plan, _ := json.Marshal(voice.OrganizationPlan{Version: voice.OrganizationVersion, Sections: []voice.Snapshot{{DictationMode: true, Polish: &voice.PolishRequest{Narrator: &voice.Narrator{PersonID: author, Name: "我"}, Targets: []voice.PolishTarget{{ID: uuid.NewString(), Text: "今天还没付款。", SourceText: "今天还没付款。", SourceIDs: []string{source}, Turns: []voice.SourceUtterance{{ID: source, Text: "今天还没付款。", PersonID: author, Person: "我"}}, Style: "body", CompleteSource: true}}}}}})
			hash := sha256.Sum256(plan)
			body, _ := json.Marshal(map[string]any{"requestID": id, "inputSHA256": hex.EncodeToString(hash[:]), "plan": plan})
			request := authorizedRequest(http.MethodPost, "/v1/journal/voice/organizations", string(body))
			if !tc.authenticated {
				request.Header.Del("X-Tellyouwhat-Assertion")
			}
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			if store.saved != (tc.status == 200) {
				t.Fatal("denied task saved or valid task lost")
			}
			if tc.status != 200 {
				return
			}
			if store.job.OwnerID != "valid-key" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("owner or cache boundary")
			}
			store.job.OwnerID = "other"
			response = httptest.NewRecorder()
			server.Router().ServeHTTP(response, authorizedRequest(http.MethodGet, "/v1/journal/voice/organizations/"+id, ""))
			if response.Code != 404 {
				t.Fatalf("cross-owner result: %d %s", response.Code, response.Body.String())
			}
			var changed map[string]any
			_ = json.Unmarshal(body, &changed)
			changed["requestID"] = uuid.NewString()
			body, _ = json.Marshal(changed)
			response = httptest.NewRecorder()
			server.Router().ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/journal/voice/organizations", string(body)))
			if response.Code != 422 {
				t.Fatalf("unbound request ID: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestJournalVoiceOrganizationCanCancelAfterConsentAndSubscriptionEnd(t *testing.T) {
	id := "19be2f9e-bd92-4699-b561-e3816092114c"
	store := &organizationStoreFixture{job: voice.OrganizationJob{ID: id, OwnerID: "valid-key", State: "running", Revision: 2, ExpiresAt: time.Now().Add(time.Hour)}}
	service := &voice.OrganizationService{Store: store, Now: time.Now}
	server := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: "journal"}, Entitlements: fakeEntitlements{allowed: false}, Consent: fakeConsentGate{granted: false}, JournalVoiceOrganizations: service})
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/journal/voice/organizations/"+id+"/cancel", `{"revision":1}`))
	if response.Code != 200 || store.job.State != "cancelled" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cancellation: %d %s", response.Code, response.Body.String())
	}
}
