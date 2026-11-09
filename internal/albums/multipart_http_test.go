package albums

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func TestMultipartHTTPAccountFlowMatchesContractAndPersistsState(t *testing.T) {
	uploads, repository, _, owner, manifest := uploadFixture(t)
	u, err := uploads.Create(context.Background(), owner, uuid.NewString(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	r := &multipartRepositoryStub{uploadTestRepository: repository}
	initializations, completions := 0, 0
	p := &multipartProviderStub{
		init: func(context.Context, string) (string, error) { initializations++; return "provider-private", nil },
		part: func(_ context.Context, key, id string, resource Resource, number int, expiry time.Time) (MultipartPartGrant, error) {
			return MultipartPartGrant{PutGrant: PutGrant{ResourceID: resource.ID, URL: "https://bucket.example/part", Headers: map[string]string{"Content-Length": "23"}, ExpiresAt: expiry}, PartNumber: number, SizeBytes: resource.SizeBytes}, nil
		},
		list: func(context.Context, string, string, int64) ([]MultipartUploadedPart, error) {
			return []MultipartUploadedPart{{1, manifest.Resources[0].SizeBytes}}, nil
		},
		complete: func(context.Context, string, string, int64) (string, error) {
			completions++
			return "object-private", nil
		},
	}
	service, _ := NewMultipartService(r, p, uploads.now)
	router, err := NewHTTPRouterWithMultipart("albums.example", testAccountAuth{identity: AccountIdentity{"albums", owner}}, uploads, service)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := "https://albums.example/v1/albums/uploads/" + u.ID + "/resources/photo/multipart"
	request := func(method, path, schema, state string) {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(method, base+path, nil))
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(response.Code, response.Body.String())
		}
		var value map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if err := doc.Components.Schemas[schema].Value.VisitJSON(value); err != nil {
			t.Fatal("response violates generated contract", err, response.Body.String())
		}
		if state != "" && value["state"] != state {
			t.Fatal("wrong phase", value)
		}
		if strings.Contains(response.Body.String(), "provider-private") || strings.Contains(response.Body.String(), "object-private") {
			t.Fatal("control state leaked")
		}
	}
	request("POST", "", "MultipartSession", "")
	request("POST", "", "MultipartSession", "")
	request("GET", "", "MultipartStatus", "uploading")
	request("POST", "/parts/1/grant", "MultipartPartGrant", "")
	request("POST", "/complete", "MultipartAssembly", "assembled")
	request("POST", "/complete", "MultipartAssembly", "assembled")
	request("GET", "", "MultipartStatus", "assembled")
	if initializations != 1 || completions != 1 {
		t.Fatal("HTTP retries repeated external mutation")
	}
	if repository.values[u.ID].State != Uploading {
		t.Fatal("assembly incorrectly declared backup")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", base+"/parts/1/grant", nil))
	if response.Code != 409 {
		t.Fatal("post-assembly grant accepted")
	}
}

func TestMultipartHTTPRejectsScopeInjectionAndUnknownOutcome(t *testing.T) {
	uploads, repository, _, owner, manifest := uploadFixture(t)
	u, _ := uploads.Create(context.Background(), owner, uuid.NewString(), manifest)
	r := &multipartRepositoryStub{uploadTestRepository: repository}
	calls := 0
	p := &multipartProviderStub{init: func(context.Context, string) (string, error) {
		calls++
		return "", errors.New("private provider failure")
	}}
	service, _ := NewMultipartService(r, p, uploads.now)
	base := "https://albums.example/v1/albums/uploads/" + u.ID + "/resources/photo/multipart"
	for _, scenario := range []struct {
		name, method, suffix, body string
		identity                   AccountIdentity
		authErr                    error
		want                       int
	}{
		{name: "body", method: "POST", body: `{"uploadId":"injected"}`, identity: AccountIdentity{"albums", owner}, want: 422},
		{name: "query", method: "POST", suffix: "?ownerID=other", identity: AccountIdentity{"albums", owner}, want: 422},
		{name: "other-owner", method: "POST", identity: AccountIdentity{"albums", uuid.NewString()}, want: 404},
		{name: "other-app", method: "POST", identity: AccountIdentity{"health", owner}, want: 403},
		{name: "unauthenticated", method: "POST", authErr: errors.New("auth failed"), want: 401},
		{name: "bad-part", method: "POST", suffix: "/parts/nope/grant", identity: AccountIdentity{"albums", owner}, want: 400},
		{name: "zero-part", method: "POST", suffix: "/parts/0/grant", identity: AccountIdentity{"albums", owner}, want: 422},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			router, err := NewHTTPRouterWithMultipart("albums.example", testAccountAuth{identity: scenario.identity, err: scenario.authErr}, uploads, service)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(scenario.method, base+scenario.suffix, strings.NewReader(scenario.body)))
			if response.Code != scenario.want || calls != 0 {
				t.Fatal("invalid request reached provider", response.Code, response.Body.String())
			}
		})
	}
	router, _ := NewHTTPRouterWithMultipart("albums.example", testAccountAuth{identity: AccountIdentity{"albums", owner}}, uploads, service)
	for range 2 {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("POST", base, nil))
		if response.Code != 503 || response.Header().Get("Retry-After") != "30" || strings.Contains(response.Body.String(), "private") {
			t.Fatal("unsafe pending response", response.Body.String())
		}
	}
	if calls != 1 {
		t.Fatal("unknown init retried")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", base, nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"state":"pending"`) {
		t.Fatal("pending state inaccessible", response.Body.String())
	}
	old, _ := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", owner}}, uploads)
	response = httptest.NewRecorder()
	old.ServeHTTP(response, httptest.NewRequest("POST", base, nil))
	if response.Code != 503 {
		t.Fatal("legacy constructor unexpectedly enabled multipart")
	}
}
