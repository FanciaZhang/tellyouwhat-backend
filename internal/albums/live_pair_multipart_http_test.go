package albums

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestMySQLLiveMultipartHTTPContractAndBoundaries(t *testing.T) {
	f, s, uploads, u, p := liveMultipartFixture(t)
	uploads.objects = p
	multipart, err := NewMultipartService(s.repository, p, s.now)
	if err != nil {
		t.Fatal(err)
	}
	auth := testAccountAuth{identity: AccountIdentity{"albums", u.OwnerID}}
	router, err := NewHTTPRouterWithMultipart("albums.example", auth, uploads, multipart)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := "https://albums.example/v1/albums/live-pair-uploads/" + u.ID + "/resources/photo/multipart"
	for _, step := range []struct{ method, path, schema string }{
		{"POST", "", "MultipartSession"}, {"POST", "", "MultipartSession"}, {"GET", "", "MultipartStatus"},
		{"POST", "/parts/1/grant", "MultipartPartGrant"}, {"POST", "/complete", "MultipartAssembly"}, {"GET", "", "MultipartStatus"},
	} {
		out := httptest.NewRecorder()
		router.ServeHTTP(out, httptest.NewRequest(step.method, base+step.path, nil))
		if out.Code != 200 || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(out.Code, out.Body.String())
		}
		var body map[string]any
		if err = json.Unmarshal(out.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if err = doc.Components.Schemas[step.schema].Value.VisitJSON(body); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.Body.String(), "intentID") || strings.Contains(out.Body.String(), "operationID") || strings.Contains(out.Body.String(), "version-provider") {
			t.Fatal("internal journal exposed")
		}
	}
	if p.initiations.Load() != 1 || p.completions.Load() != 1 {
		t.Fatal("duplicate provider operation")
	}
	for _, invalid := range []struct{ method, path, body string }{
		{"POST", base, "{}"}, {"POST", base + "?provider=other", ""}, {"POST", base + "/parts/0/grant", ""},
		{"POST", strings.Replace(base, "/photo/", "/../", 1), ""},
	} {
		out := httptest.NewRecorder()
		router.ServeHTTP(out, httptest.NewRequest(invalid.method, invalid.path, strings.NewReader(invalid.body)))
		if out.Code < 400 {
			t.Fatal("malformed request accepted", out.Code)
		}
	}
	deleteProtocolFixtureOwner(t, f, u.OwnerID)
	out := httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest("GET", base, nil))
	if out.Code < 400 {
		t.Fatal("deleting account received status")
	}
	if p.initiations.Load() != 1 || p.completions.Load() != 1 {
		t.Fatal("rejected requests reached provider mutation")
	}
}
