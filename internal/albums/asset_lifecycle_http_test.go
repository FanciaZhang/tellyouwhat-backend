package albums

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func TestMySQLCloudLifecycleHTTPContractsAndBoundaries(t *testing.T) {
	f := newSharingFixture(t)
	limiter := &loginHTTPLimiter{}
	options := &LoginHTTPOptions{TrashEnabled: true, LibraryLimiter: limiter}
	auth := testAccountAuth{identity: AccountIdentity{"albums", f.login.AccountID}}
	router, err := newHTTPRouter("albums.example", auth, f.uploads, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/albums/library/assets/" + f.version.AssetID
	send := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "https://albums.example"+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Forwarded-For", "198.51.100.2")
		request.RemoteAddr = "192.0.2.1:1234"
		out := httptest.NewRecorder()
		router.ServeHTTP(out, request)
		return out
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	validate := func(out *httptest.ResponseRecorder, schema string) {
		t.Helper()
		if out.Code != 200 || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(out.Code, out.Body.String())
		}
		var value any
		if err := json.Unmarshal(out.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if err := doc.Components.Schemas[schema].Value.VisitJSON(value); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"albums/originals/", "immutable-share-v1", "lifecycle_epoch"} {
			if strings.Contains(out.Body.String(), secret) {
				t.Fatal("provider proof exposed")
			}
		}
	}
	validate(send("GET", path, ""), "CloudAssetState")
	validate(send("GET", "/v1/albums/library/changes?after=0&limit=1", ""), "LibraryChangePage")
	input := `{"requestID":"` + uuid.NewString() + `","expectedRevision":1}`
	options.TrashEnabled = false
	if got := send("POST", path+"/trash", input); got.Code != 503 {
		t.Fatal("trash default flag ignored", got.Code)
	}
	options.TrashEnabled = true
	limiter.delay = time.Second
	if got := send("POST", path+"/trash", input); got.Code != 429 || got.Header().Get("Retry-After") != "1" {
		t.Fatal("limiter", got.Code)
	}
	limiter.delay = 0
	for _, bad := range []string{
		`null`, `{}`, input + `{}`, strings.Replace(input, `"expectedRevision":1`, `"expectedRevision":null`, 1),
		strings.Replace(input, `"expectedRevision":1`, `"expectedRevision":0`, 1),
		strings.Replace(input, `"expectedRevision":1`, `"expectedRevision":1,"expectedRevision":1`, 1),
		strings.Replace(input, `"expectedRevision":1`, `"expectedRevision":1,"ownerID":"other"`, 1),
		strings.Repeat(" ", 4096) + input,
	} {
		if got := send("POST", path+"/trash", bad); got.Code != 422 {
			t.Fatal("invalid mutation", got.Code)
		}
	}
	for _, suffix := range []string{"?after=0&after=1", "?owner=other", "?limit=", "?limit=101", "?after=-1"} {
		if got := send("GET", "/v1/albums/library/changes"+suffix, ""); got.Code < 400 || got.Code >= 500 {
			t.Fatal("invalid query", got.Code)
		}
	}
	if got := send("GET", path, "{}"); got.Code != 422 {
		t.Fatal("GET body", got.Code)
	}
	if got := send("POST", path+"/trash?owner=other", input); got.Code != 422 {
		t.Fatal("mutation query", got.Code)
	}
	validate(send("POST", path+"/trash", input), "CloudAssetState")
	validate(send("POST", path+"/trash", input), "CloudAssetState")
	validate(send("GET", "/v1/albums/library/changes?after=0", ""), "LibraryChangePage")
	if got := send("GET", "/v1/albums/library/versions/"+f.version.VersionID, ""); got.Code != 404 {
		t.Fatal("direct read bypassed trash", got.Code)
	}
	restore := `{"requestID":"` + uuid.NewString() + `","expectedRevision":2}`
	validate(send("POST", path+"/restore", restore), "CloudAssetState")
	if limiter.address != "192.0.2.1" || limiter.operation != "library-lifecycle" {
		t.Fatal("spoofable limiter")
	}
	for _, principal := range []AccountIdentity{{"albums", uuid.NewString()}, {"health", f.login.AccountID}} {
		foreign, err := newHTTPRouter("albums.example", testAccountAuth{identity: principal}, f.uploads, options, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := httptest.NewRecorder()
		foreign.ServeHTTP(out, httptest.NewRequest("GET", "https://albums.example"+path, nil))
		if out.Code != 404 && out.Code != 403 {
			t.Fatal("foreign asset accessible", out.Code)
		}
	}
}

func TestGatewayTrashRequiresExplicitValidFlag(t *testing.T) {
	values := gatewayEnvironment(t)
	for _, value := range []string{"", "false", "true", "1", "TRUE"} {
		values["ALBUM_TRASH_ENABLED"] = value
		config, err := LoadGatewayConfig(func(k string) string { return values[k] })
		if value == "1" || value == "TRUE" {
			if err == nil {
				t.Fatal("ambiguous enable flag")
			}
			continue
		}
		if err != nil || config.TrashEnabled != (value == "true") {
			t.Fatal("trash flag", err)
		}
	}
}
