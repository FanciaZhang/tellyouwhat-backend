package albums

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type loginHTTPLimiter struct {
	delay              time.Duration
	err                error
	address, operation string
	calls              int
}

func (l *loginHTTPLimiter) Check(_ context.Context, operation, address string) (time.Duration, error) {
	l.calls++
	l.address = address
	l.operation = operation
	return l.delay, l.err
}

type loginHTTPRevoker struct {
	token string
	err   error
}

func (r *loginHTTPRevoker) Revoke(_ context.Context, token string) error {
	r.token = token
	return r.err
}

func TestLoginHTTPPublicBoundariesAndLimits(t *testing.T) {
	uploads, _, _, _, _ := uploadFixture(t)
	fixture := &loginPipelineFixture{}
	challengeStore := &challengeTestStore{rows: map[string]StoredLoginChallenge{}, used: map[string]bool{}}
	challenges, _ := NewLoginChallengeService(challengeStore, time.Now)
	login, _ := NewLoginService(challenges, fixture, fixture, fixture)
	limiter := &loginHTTPLimiter{}
	options := LoginHTTPOptions{Service: login, Limiter: limiter, Sessions: &loginHTTPRevoker{}}
	router, err := NewHTTPRouterWithLogin("albums.example", testAccountAuth{err: ErrSession}, uploads, options)
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://albums.example"+path, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.10:1234"
		r.Header.Set("X-Forwarded-For", "198.51.100.50")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	response := send("POST", "/v1/albums/auth/challenges", "")
	if response.Code != 201 || response.Header().Get("Cache-Control") != "no-store" || limiter.address != "192.0.2.10" {
		t.Fatal("public challenge or proxy trust", response.Code, limiter.address)
	}
	var challenge LoginChallenge
	if err := json.Unmarshal(response.Body.Bytes(), &challenge); err != nil {
		t.Fatal(err)
	}
	fixture.nonce = challenge.Nonce
	request := AppleLoginRequest{challenge.ID, challenge.Proof, "code", "identity"}
	encoded, _ := json.Marshal(request)
	// Unknown fields must be rejected before consuming the challenge.
	injection := strings.TrimSuffix(string(encoded), "}") + `,"accountID":"forged"}`
	if w := send("POST", "/v1/albums/auth/apple", injection); w.Code != 400 {
		t.Fatal("account injection accepted", w.Code)
	}
	if w := send("POST", "/v1/albums/auth/apple", string(encoded)+` {}`); w.Code != 400 {
		t.Fatal("trailing body accepted")
	}
	if w := send("POST", "/v1/albums/auth/apple", `{"identityToken":"`+strings.Repeat("x", 81<<10)+`"}`); w.Code != 400 {
		t.Fatal("oversized body accepted")
	}
	limiter.delay = 1500 * time.Millisecond
	if w := send("POST", "/v1/albums/auth/apple", string(encoded)); w.Code != 429 || w.Header().Get("Retry-After") != "2" || len(fixture.steps) != 0 {
		t.Fatal("rate limit bypassed")
	}
	limiter.delay = 0
	limiter.err = errors.New("private Redis details")
	if w := send("POST", "/v1/albums/auth/apple", string(encoded)); w.Code != 503 || strings.Contains(w.Body.String(), "private") || len(fixture.steps) != 0 {
		t.Fatal("limiter unavailable bypassed")
	}
	limiter.err = nil
	if w := send("POST", "/v1/albums/auth/apple", string(encoded)); w.Code != 200 || !strings.Contains(w.Body.String(), "private-session") {
		t.Fatal("login failed", w.Code, w.Body.String())
	}
	if w := send("POST", "/v1/albums/auth/apple", string(encoded)); w.Code != 401 {
		t.Fatal("replay accepted")
	}
	for _, path := range []string{"/v1/albums/uploads", "/v1/albums/auth/logout", "/v1/albums/auth/apple/"} {
		if w := send("POST", path, ""); w.Code != 401 {
			t.Fatal("protected path bypassed", path, w.Code)
		}
	}
	if w := send("GET", "/v1/albums/account", ""); w.Code != 401 {
		t.Fatal("anonymous account accepted")
	}
	before := limiter.calls
	bad := httptest.NewRequest("POST", "https://other.example/v1/albums/auth/challenges", nil)
	bad.Header.Set("X-Forwarded-Host", "albums.example")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, bad)
	if w.Code != 421 || limiter.calls != before {
		t.Fatal("host gate bypassed")
	}
	// Only an explicitly trusted immediate peer may supply forwarded addresses.
	options.TrustedProxies = []string{"192.0.2.10/32"}
	router, err = NewHTTPRouterWithLogin("albums.example", testAccountAuth{err: ErrSession}, uploads, options)
	if err != nil {
		t.Fatal(err)
	}
	if w := send("POST", "/v1/albums/auth/challenges", ""); w.Code != 201 || limiter.address != "198.51.100.50" {
		t.Fatal("configured proxy ignored")
	}
}

func TestLoginHTTPAccountAndLogout(t *testing.T) {
	uploads, _, _, owner, _ := uploadFixture(t)
	fixture := &loginPipelineFixture{}
	challenges, _ := NewLoginChallengeService(&challengeTestStore{rows: map[string]StoredLoginChallenge{}, used: map[string]bool{}}, time.Now)
	login, _ := NewLoginService(challenges, fixture, fixture, fixture)
	revoker := &loginHTTPRevoker{}
	router, err := NewHTTPRouterWithLogin("albums.example", testAccountAuth{identity: AccountIdentity{"albums", owner}}, uploads, LoginHTTPOptions{Service: login, Limiter: &loginHTTPLimiter{}, Sessions: revoker})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "https://albums.example/v1/albums/account", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), owner) {
		t.Fatal("account response", w.Code)
	}
	token := "als1_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	r := httptest.NewRequest("POST", "https://albums.example/v1/albums/auth/logout", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 204 || revoker.token != token || w.Body.Len() != 0 {
		t.Fatal("logout failed", w.Code)
	}
}
