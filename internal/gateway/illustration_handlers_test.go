package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

type illustrationProofRecorder struct{ proof RequestProof }

func (r *illustrationProofRecorder) Authenticate(ctx context.Context, proof RequestProof) (Principal, error) {
	r.proof = proof
	return (fakeAuthenticator{appID: "journal"}).Authenticate(ctx, proof)
}

func TestIllustrationBindsRawBodyAndFailsClosedWithoutService(t *testing.T) {
	recorder := &illustrationProofRecorder{}
	service := &illustrationAPIStub{}
	s := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: recorder, JournalIllustrations: service})
	response := httptest.NewRecorder()
	s.Router().ServeHTTP(response, authorizedRequest("POST", "/v1/journal/illustrations", illustrationBody))
	if response.Code != 200 || recorder.proof.BodySHA256 != contracts.BodySHA256([]byte(illustrationBody)) || recorder.proof.Path != "/v1/journal/illustrations" {
		t.Fatal("image payload was not authenticated")
	}
	s = New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: recorder})
	response = httptest.NewRecorder()
	s.Router().ServeHTTP(response, authorizedRequest("POST", "/v1/journal/illustrations", illustrationBody))
	if response.Code != 503 {
		t.Fatalf("missing service did not fail closed: %d", response.Code)
	}
}

const illustrationID = "19be2f9e-bd92-4699-b561-e3816092114c"
const illustrationBody = `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c","versionID":"8d43cd74-5652-4412-b097-303f563e673a","prompt":"一棵树","consentRevision":"2026-09-27"}`

type illustrationAPIStub struct {
	calls int
	err   error
	owner string
}

func (f *illustrationAPIStub) task(p Principal) (illustration.Task, error) {
	f.calls++
	f.owner = p.KeyID
	return illustration.Task{ID: illustrationID, VersionID: "8d43cd74-5652-4412-b097-303f563e673a", State: illustration.TaskQueued, Revision: 1, ExpiresAt: time.Unix(1000, 0), BillingAccount: "private-account", RequestDigest: "private-digest"}, f.err
}
func (f *illustrationAPIStub) Create(_ context.Context, p Principal, _ illustration.Confirmation, _ []byte) (illustration.Task, error) {
	return f.task(p)
}
func (f *illustrationAPIStub) Status(_ context.Context, p Principal, _ string) (illustration.Task, error) {
	return f.task(p)
}
func (f *illustrationAPIStub) Cancel(_ context.Context, p Principal, _ string, _ uint64) (illustration.Task, error) {
	return f.task(p)
}
func (f *illustrationAPIStub) Result(_ context.Context, p Principal, _ string) (illustration.Result, error) {
	f.calls++
	f.owner = p.KeyID
	return illustration.Result{Image: []byte{1, 2, 3}, MIME: "image/png", Width: 1, Height: 1}, f.err
}

func TestIllustrationHTTPRoutes(t *testing.T) {
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/v1/journal/illustrations", illustrationBody},
		{"GET", "/v1/journal/illustrations/" + illustrationID, ""},
		{"POST", "/v1/journal/illustrations/" + illustrationID + "/cancel", `{"revision":1}`},
		{"GET", "/v1/journal/illustrations/" + illustrationID + "/result", ""},
	} {
		t.Run(route.path, func(t *testing.T) {
			service := &illustrationAPIStub{}
			s := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: "journal"}, JournalIllustrations: service})
			response := httptest.NewRecorder()
			s.Router().ServeHTTP(response, authorizedRequest(route.method, route.path, route.body))
			if response.Code != 200 || service.calls != 1 || service.owner != "valid-key" {
				t.Fatalf("%d %s calls=%d", response.Code, response.Body.String(), service.calls)
			}
			if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "private-") {
				t.Fatal("private metadata/cache leak")
			}
			unauth := authorizedRequest(route.method, route.path, route.body)
			unauth.Header.Del("X-Tellyouwhat-Assertion")
			response = httptest.NewRecorder()
			s.Router().ServeHTTP(response, unauth)
			if response.Code != 401 || service.calls != 1 {
				t.Fatal("unauthenticated service invocation")
			}
			health := New(Dependencies{App: appregistry.App{ID: appregistry.Health}, Authenticator: fakeAuthenticator{}, JournalIllustrations: service})
			response = httptest.NewRecorder()
			health.Router().ServeHTTP(response, authorizedRequest(route.method, route.path, route.body))
			if response.Code != 404 || service.calls != 1 {
				t.Fatalf("Health route exposed: %d", response.Code)
			}
		})
	}
}

func TestIllustrationHTTPRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"unknown", "/v1/journal/illustrations", strings.TrimSuffix(illustrationBody, "}") + `,"ownerID":"other"}`, 422},
		{"mismatch", "/v1/journal/illustrations", strings.Replace(illustrationBody, illustrationID, "29be2f9e-bd92-4699-b561-e3816092114c", 1), 401},
		{"negative", "/v1/journal/illustrations/" + illustrationID + "/cancel", `{"revision":-1}`, 422},
		{"oversize-cancel", "/v1/journal/illustrations/" + illustrationID + "/cancel", strings.Repeat(" ", 1025), 413},
		{"oversize-image", "/v1/journal/illustrations", strings.Repeat(" ", 34_000_001), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &illustrationAPIStub{}
			s := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: "journal"}, JournalIllustrations: service})
			response := httptest.NewRecorder()
			s.Router().ServeHTTP(response, authorizedRequest("POST", tc.path, tc.body))
			if response.Code != tc.status || service.calls != 0 {
				t.Fatalf("%d %s calls=%d", response.Code, response.Body.String(), service.calls)
			}
		})
	}
}

func TestIllustrationHTTPErrorRedaction(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{errors.New("private prompt database credentials"), 503}, {illustration.ErrAccessDenied, 403}, {illustration.ErrTaskConflict, 409}, {illustration.ErrImageBudget, 429},
	} {
		s := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: "journal"}, JournalIllustrations: &illustrationAPIStub{err: tc.err}})
		response := httptest.NewRecorder()
		s.Router().ServeHTTP(response, authorizedRequest(http.MethodGet, "/v1/journal/illustrations/"+illustrationID, ""))
		if response.Code != tc.status || strings.Contains(response.Body.String(), "private") {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
	}
}
