package albums

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func TestGatewayAlbumSharingRealSessionsRedisCORSAndPinnedAttachments(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	env := gatewayEnvironment(t)
	env["ALBUM_DATABASE_DSN"] = f.dsn
	env["ALBUM_REDIS_URL"] = os.Getenv("REDIS_TEST_URL")
	config, err := LoadGatewayConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	config.Sharing = &AlbumShareConfig{SiteOrigin: "https://shares.example", Keys: [][]byte{f.key}}
	transport := appleTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme == "https" && request.URL.Host == config.Worker.COS.Bucket+".cos."+config.Worker.COS.Region+".myqcloud.com" && request.URL.Query().Has("versioning") {
			return cosResponse(request, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, ""), nil
		}
		t.Error("unexpected provider operation")
		return nil, ErrCOSOperation
	})
	gateway, err := NewGateway(ctx, config, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gateway.Close)
	server := httptest.NewServer(gateway)
	t.Cleanup(server.Close)
	send := func(method, path, credential, origin string, value any, headers map[string]string) (int, http.Header, []byte) {
		var body []byte
		if value != nil {
			body, _ = json.Marshal(value)
		}
		request, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Host = config.APIHost
		request.Header.Set("Content-Type", "application/json")
		if credential != "" {
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 128<<10))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("private-signing-detail")) {
			t.Fatal("private error leaked")
		}
		return response.StatusCode, response.Header.Clone(), data
	}
	path := "/v1/albums/library/versions/" + f.version.VersionID + "/share-links"
	input := ShareCreateInput{uuid.NewString(), 86400, 1, "秘密123"}
	if status, _, _ := send("POST", path, "", "", input, nil); status != 401 {
		t.Fatal("owner create did not require session", status)
	}
	status, _, data := send("POST", path, f.session.Token, "", input, nil)
	if status != 201 {
		t.Fatal("create through assembled gateway", status, string(data))
	}
	var link ShareLink
	if json.Unmarshal(data, &link) != nil || link.VersionID != f.version.VersionID {
		t.Fatal("link response")
	}
	parsed, _ := url.Parse(link.URL)
	token := parsed.Fragment
	// A genuine, different account/session still cannot manage this owner's link.
	cipher, _ := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32)))
	accounts, _ := NewMySQLAccounts(f.db, cipher)
	foreign, err := accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: freshAppleIdentity(uuid.NewString()), RefreshToken: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := NewMySQLSessions(f.db, time.Now)
	foreignSession, err := sessions.Issue(ctx, foreign)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		var body any
		if method == "POST" {
			body = input
		}
		if status, _, _ := send(method, path, foreignSession.Token, "", body, nil); status != 404 {
			t.Fatal("foreign account managed owner version", method, status)
		}
	}
	if status, _, _ := send("DELETE", "/v1/albums/share-links/"+link.ID, foreignSession.Token, "", nil, nil); status != 404 {
		t.Fatal("foreign account revoked owner link", status)
	}
	status, headers, _ := send("OPTIONS", "/v1/albums/share-links/lookup", "", "https://shares.example", nil, map[string]string{"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type"})
	if status != 204 || headers.Get("Access-Control-Allow-Origin") != "https://shares.example" || headers.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("recipient preflight", status)
	}
	if status, _, _ := send("OPTIONS", "/v1/albums/share-links/lookup", "", "https://shares.example", nil, map[string]string{"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization"}); status != 403 {
		t.Fatal("account header admitted in preflight", status)
	}
	if status, _, _ := send("POST", "/v1/albums/share-links/lookup", "", "https://evil.example", map[string]string{"token": token}, nil); status != 403 {
		t.Fatal("foreign origin", status)
	}
	if status, _, _ := send("GET", "/v1/albums/share-links/lookup", "", "", nil, nil); status != 401 {
		t.Fatal("public scope exceeded exact POST", status)
	}
	if status, _, _ := send("POST", "/v1/albums/share-links/lookup?owner=forged", "", "", map[string]string{"token": token}, nil); status != 422 {
		t.Fatal("query owner injection", status)
	}
	if status, _, _ := send("POST", "/v1/albums/share-links/lookup", "", "", map[string]string{"token": token, "ownerID": uuid.NewString()}, nil); status != 422 {
		t.Fatal("body owner injection", status)
	}
	status, headers, data = send("POST", "/v1/albums/share-links/lookup", "", "https://shares.example", map[string]string{"token": token}, nil)
	if status != 200 || headers.Get("Cache-Control") != "no-store" || bytes.Contains(data, []byte(f.login.AccountID)) || bytes.Contains(data, []byte("albums/originals/")) {
		t.Fatal("lookup leaked server-only metadata", status)
	}
	requestID := uuid.NewString()
	claimInput := map[string]string{"token": token, "password": input.Password, "requestID": requestID}
	status, _, data = send("POST", "/v1/albums/share-links/claims", "", "https://shares.example", claimInput, nil)
	if status != 200 {
		t.Fatal("claim", status)
	}
	var claim ShareClaim
	if json.Unmarshal(data, &claim) != nil || len(claim.Token) != 80 {
		t.Fatal("claim response")
	}
	if status, _, data = send("POST", "/v1/albums/share-links/claims", "", "", claimInput, nil); status != 200 {
		t.Fatal("claim retry", status)
	}
	var repeated ShareClaim
	json.Unmarshal(data, &repeated)
	if repeated.Token != claim.Token {
		t.Fatal("gateway retry charged extra count")
	}
	status, _, data = send("POST", "/v1/albums/share-links/read-grants", "", "https://shares.example", map[string]string{"token": claim.Token}, nil)
	if status != 200 {
		t.Fatal("public read grants", status)
	}
	var result struct {
		Resources []SharedReadGrant `json:"resources"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Resources) != 1 {
		t.Fatal("grants response")
	}
	grant := result.Resources[0]
	signed, err := url.Parse(grant.URL)
	if err != nil {
		t.Fatal(err)
	}
	if signed.Scheme != "https" || signed.Host != config.Worker.COS.Bucket+".cos."+config.Worker.COS.Region+".myqcloud.com" || signed.Query().Get("versionId") != "immutable-share-v1" || !strings.Contains(signed.Query().Get("q-url-param-list"), "response-content-disposition") || grant.ExpiresAt.After(time.Now().Add(5*time.Minute)) {
		t.Fatal("unpinned or unsigned attachment parameters")
	}
	kind, params, err := mime.ParseMediaType(signed.Query().Get("response-content-disposition"))
	if err != nil || kind != "attachment" || params["filename"] != "回忆.jpeg" {
		t.Fatal("unicode filename not recoverable")
	}
	if status, _, _ := send("DELETE", "/v1/albums/share-links/"+link.ID, f.session.Token, "", nil, nil); status != 204 {
		t.Fatal("owner revoke", status)
	}
	if status, _, _ := send("POST", "/v1/albums/share-links/read-grants", "", "", map[string]string{"token": claim.Token}, nil); status != 404 {
		t.Fatal("revoked gateway granted media", status)
	}
	limited := false
	for i := 0; i < 11; i++ {
		status, headers, data := send("POST", "/v1/albums/share-links/claims", "", "https://shares.example", claimInput, nil)
		if status == 429 {
			if headers.Get("Retry-After") == "" || headers.Get("Access-Control-Expose-Headers") != "Retry-After" || !bytes.Contains(data, []byte("share_rate_limited")) {
				t.Fatal("recipient rate limit contract")
			}
			limited = true
			break
		}
		if status != 404 {
			t.Fatal("revoked claim status", status)
		}
	}
	if !limited {
		t.Fatal("real Redis recipient budget was bypassed")
	}
	gateway.Close()
	if status, _, _ := send("POST", "/v1/albums/share-links/lookup", "", "", map[string]string{"token": token}, nil); status != 503 {
		t.Fatal("drained gateway admitted share", status)
	}
}
