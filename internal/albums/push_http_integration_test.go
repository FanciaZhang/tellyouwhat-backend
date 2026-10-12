package albums

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func TestGatewayAlbumPushRegistrationHTTPAndRevocation(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	env := gatewayEnvironment(t)
	env["ALBUM_DATABASE_DSN"], env["ALBUM_REDIS_URL"] = f.dsn, os.Getenv("REDIS_TEST_URL")
	env["ALBUM_PUSH_ENVIRONMENT"] = "production"
	config, err := LoadGatewayConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	transport := appleTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme == "https" && r.URL.Host == config.Worker.COS.Bucket+".cos."+config.Worker.COS.Region+".myqcloud.com" && r.URL.Query().Has("versioning") {
			return cosResponse(r, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, ""), nil
		}
		t.Error("registration contacted an unexpected provider")
		return nil, ErrCOSOperation
	})
	gateway, err := NewGateway(ctx, config, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gateway.Close)
	server := httptest.NewServer(gateway)
	t.Cleanup(server.Close)
	owner, _ := f.login(t, "push-http-owner", f.now)
	foreign, _ := f.login(t, "push-http-foreign", f.now.Add(time.Second))
	const path = "/v1/albums/notifications/registration"
	send := func(method, path, token, body string, headers map[string]string) (int, http.Header, []byte) {
		r, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Host = config.APIHost
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range headers {
			if k == "Host" {
				r.Host = v
			} else {
				r.Header.Set(k, v)
			}
		}
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 8192))
		if err != nil {
			t.Fatal(err)
		}
		if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Access-Control-Allow-Origin") != "" || strings.Contains(string(data), strings.Repeat("ab", 32)) {
			t.Fatal("registration response leaked token or cache/CORS permission")
		}
		return response.StatusCode, response.Header, data
	}
	for _, method := range []string{"GET", "PUT"} {
		if status, _, _ := send(method, path, "", "", nil); status != 401 {
			t.Fatal("unauthenticated registration", status)
		}
	}
	if status, _, _ := send("GET", path, owner.Token, "", map[string]string{"Host": "health.example"}); status != 421 {
		t.Fatal("foreign host admitted", status)
	}
	status, _, data := send("GET", path, owner.Token, "", nil)
	var initial albumPushRegistration
	if status != 200 || json.Unmarshal(data, &initial) != nil || initial.State != "disabled" || initial.Revision != 0 || initial.ID != "" {
		t.Fatal("initial state", status)
	}
	u := pushUpdate(strings.Repeat("ab", 32), 0)
	raw, _ := json.Marshal(u)
	status, _, data = send("PUT", path, owner.Token, string(raw), nil)
	var registered albumPushRegistration
	if status != 200 || json.Unmarshal(data, &registered) != nil || registered.State != "enabled" || registered.Revision != 1 {
		t.Fatal("registration", status)
	}
	if status, _, replay := send("PUT", path, owner.Token, string(raw), nil); status != 200 || string(replay) != string(data) {
		t.Fatal("request replay", status)
	}
	u.RequestID = uuid.NewString()
	raw, _ = json.Marshal(u)
	if status, _, data := send("PUT", path, owner.Token, string(raw), nil); status != 409 || !strings.Contains(string(data), "push_registration_changed") {
		t.Fatal("stale revision accepted", status)
	}
	if status, _, data := send("GET", path, foreign.Token, "", nil); status != 200 || strings.Contains(string(data), registered.ID) {
		t.Fatal("foreign session read registration", status)
	}
	for _, invalid := range []string{
		`{}`, `null`, `[]`, string(raw) + `{}`, strings.Repeat(" ", 4097) + string(raw),
		strings.Replace(string(raw), `"enabled":true`, `"enabled":null`, 1),
		strings.Replace(string(raw), `"enabled":true`, `"enabled":true,"enabled":false`, 1),
		strings.Replace(string(raw), `"enabled":true,`, ``, 1),
		strings.Replace(string(raw), `"expectedRevision":0`, `"expectedRevision":null`, 1),
		strings.Replace(string(raw), `"expectedRevision":0`, `"expectedRevision":-1`, 1),
		strings.Replace(string(raw), `"expectedRevision":0`, `"expectedRevision":9223372036854775807`, 1),
		strings.Replace(string(raw), `"deviceToken":`, `"accountID":`, 1),
		strings.Replace(string(raw), strings.Repeat("ab", 32), `ABCD`, 1),
		strings.Replace(string(raw), `"enabled":true`, `"enabled":false`, 1),
	} {
		if status, _, _ := send("PUT", path, owner.Token, invalid, nil); status != 400 {
			t.Fatal("invalid registration body accepted", status)
		}
	}
	if status, _, _ := send("PUT", path, owner.Token, string(raw), map[string]string{"Content-Type": "text/plain"}); status != 400 {
		t.Fatal("non-JSON accepted", status)
	}
	if status, _, _ := send("GET", path+"?owner=forged", owner.Token, "", nil); status != 400 {
		t.Fatal("query authority accepted", status)
	}
	if status, _, _ := send("GET", path, owner.Token, "{}", nil); status != 400 {
		t.Fatal("GET body accepted", status)
	}
	disable, _ := json.Marshal(pushUpdate("", 1))
	if status, _, _ := send("PUT", path, owner.Token, string(disable), nil); status != 200 {
		t.Fatal("disable", status)
	}
	u = pushUpdate(strings.Repeat("ab", 32), 2)
	raw, _ = json.Marshal(u)
	if status, _, _ := send("PUT", path, owner.Token, string(raw), nil); status != 200 {
		t.Fatal("explicit reenable", status)
	}
	if status, _, _ := send("POST", "/v1/albums/auth/logout", owner.Token, "", nil); status != 204 {
		t.Fatal("logout", status)
	}
	if status, _, _ := send("GET", path, owner.Token, "", nil); status != 401 {
		t.Fatal("revoked session read registration", status)
	}
	cipher, _ := mysqlstore.NewPayloadCipher(config.CredentialKey)
	store, _ := newAlbumPushRegistrations(f.r.db, cipher, "production")
	if _, err := store.target(ctx, owner.AccountID, registered.ID, 3); err == nil {
		t.Fatal("HTTP logout retained delivery eligibility")
	}
	limited := false
	for range 61 {
		status, headers, _ := send("GET", path, foreign.Token, "", map[string]string{"X-Forwarded-For": "203.0.113.7"})
		if status == 429 {
			if headers.Get("Retry-After") == "" {
				t.Fatal("missing retry delay")
			}
			limited = true
			break
		}
		if status != 200 {
			t.Fatal("unexpected rate-limit response", status)
		}
	}
	if !limited {
		t.Fatal("rate limit or proxy boundary bypassed")
	}
	config.PushEnvironment = ""
	disabled, err := NewGateway(ctx, config, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Close()
	r := httptest.NewRequest("GET", "https://"+config.APIHost+path, nil)
	r.Header.Set("Authorization", "Bearer "+foreign.Token)
	w := httptest.NewRecorder()
	disabled.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("registration was implicitly enabled", w.Code)
	}
}

func TestAlbumPushEnvironmentIsExplicitAndIndependent(t *testing.T) {
	env := gatewayEnvironment(t)
	for _, value := range []string{"", "development", "production"} {
		env["ALBUM_PUSH_ENVIRONMENT"] = value
		c, err := LoadGatewayConfig(func(k string) string { return env[k] })
		if err != nil || c.PushEnvironment != value {
			t.Fatal("explicit push environment", value, err)
		}
	}
	for _, value := range []string{"Sandbox", "Production", "true", "https://other.example"} {
		env["ALBUM_PUSH_ENVIRONMENT"] = value
		if _, err := LoadGatewayConfig(func(k string) string { return env[k] }); err == nil {
			t.Fatal("invalid push environment")
		}
	}
}
