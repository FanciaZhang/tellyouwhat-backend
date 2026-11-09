package albums

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const deletionPath = "/v1/albums/account/deletion"

func deletionRouter(t *testing.T, f *pushFixture, limiter *loginHTTPLimiter, enabled bool) http.Handler {
	t.Helper()
	uploads, _, _, _, _ := uploadFixture(t)
	pipeline := &loginPipelineFixture{}
	challenges, _ := NewLoginChallengeService(&challengeTestStore{rows: map[string]StoredLoginChallenge{}, used: map[string]bool{}}, time.Now)
	login, _ := NewLoginService(challenges, pipeline, pipeline, pipeline)
	opts := LoginHTTPOptions{Service: login, Limiter: limiter, Sessions: f.sessions}
	if enabled {
		opts.AccountDeletion = &accountDeletionHTTP{f.accounts, func() time.Time { return f.now }}
	}
	router, err := NewHTTPRouterWithLogin("albums.example", f.sessions, uploads, opts)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func sendDeletion(router http.Handler, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://albums.example"+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-For", "198.51.100.50")
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
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func TestMySQLAccountDeletionHTTPAcceptsAndReplaysAfterAccessErasure(t *testing.T) {
	f := newPushFixture(t)
	first, g1 := f.login(t, "erasure-owner", f.now)
	second, g2 := f.login(t, "erasure-owner", f.now)
	other, g3 := f.login(t, "keep-owner", f.now)
	limiter := &loginHTTPLimiter{}
	router := deletionRouter(t, f, limiter, true)
	id := uuid.NewString()
	body := `{"requestID":"` + id + `"}`
	w := sendDeletion(router, "POST", deletionPath, first.Token, body, nil)
	if w.Code != 202 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Access-Control-Allow-Origin") != "" || limiter.operation != "account-deletion" || limiter.address != "192.0.2.10" {
		t.Fatal(w.Code, w.Body.String(), limiter)
	}
	var receipt struct {
		RequestID   string    `json:"requestID"`
		RequestedAt time.Time `json:"requestedAt"`
		State       string    `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || receipt.RequestID != id || receipt.State != "accepted" || !receipt.RequestedAt.Equal(f.now) || strings.Contains(w.Body.String(), first.AccountID) || strings.Contains(w.Body.String(), first.Token) {
		t.Fatal("invalid acceptance receipt", err)
	}
	if deletionAuthenticated(f, first.Token) || deletionAuthenticated(f, second.Token) || !deletionAuthenticated(f, other.Token) {
		t.Fatal("access was not cut over within acceptance")
	}
	for _, input := range []struct{ token, body string }{{second.Token, body}, {first.Token, `{"requestID":"` + uuid.NewString() + `"}`}, {other.Token, body}} {
		reply := sendDeletion(router, "POST", deletionPath, input.token, input.body, nil)
		if reply.Code == 202 || strings.Contains(reply.Body.String(), first.AccountID) || strings.Contains(reply.Body.String(), id) {
			t.Fatal("foreign request accepted or disclosed", reply.Code)
		}
	}
	if !deletionAuthenticated(f, other.Token) {
		t.Fatal("colliding request revoked unrelated account")
	}
	d := accountDeletionRequest{first.AccountID, id, f.now}
	x := accessErasureFixture{f, d, first, second, other, []AccountLogin{g1, g2, g3}}
	x.finishMedia(t)
	x.revoke(t)
	if _, err := f.accounts.eraseAccountAccess(context.Background(), d.AccountID, id, f.now); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.AddDate(1, 0, 0)
	// A new router and erased session records cannot turn a lost response into
	// another request or a false claim that physical erasure has completed.
	reopened := deletionRouter(t, f, &loginHTTPLimiter{}, true)
	replay := sendDeletion(reopened, "POST", deletionPath, first.Token, body, nil)
	if replay.Code != 202 || replay.Body.String() != w.Body.String() {
		t.Fatal("exact replay failed", replay.Code, replay.Body.String())
	}
	if reply := sendDeletion(reopened, "POST", "/v1/albums/uploads", first.Token, "{}", nil); reply.Code != 401 {
		t.Fatal("deletion replay credential regained media access", reply.Code)
	}
}

func TestMySQLAccountDeletionHTTPRejectsMalformedRequestsBeforeMutation(t *testing.T) {
	f := newPushFixture(t)
	s, _ := f.login(t, "owner", f.now)
	router := deletionRouter(t, f, &loginHTTPLimiter{}, true)
	id := uuid.NewString()
	valid := `{"requestID":"` + id + `"}`
	for _, body := range []string{"", "null", "[]", "{}", `{"requestID":null}`, `{"requestID":1}`, `{"requestID":"BAD"}`, `{"requestID":"00000000-0000-0000-0000-000000000000"}`, `{"requestID":"` + strings.ToUpper(id) + `"}`, valid + "{}", valid + strings.Repeat(" ", 512), strings.TrimSuffix(valid, "}") + `,"requestID":"` + id + `"}`, strings.TrimSuffix(valid, "}") + `,"accountID":"` + s.AccountID + `"}`} {
		if w := sendDeletion(router, "POST", deletionPath, s.Token, body, nil); w.Code != 400 {
			t.Fatal("malformed deletion accepted", w.Code, body)
		}
	}
	for _, c := range []struct {
		path, token string
		headers     map[string]string
		status      int
	}{
		{deletionPath + "?accountID=" + s.AccountID, s.Token, nil, 400},
		{deletionPath, "", nil, 401},
		{deletionPath, "invalid", nil, 401},
		{deletionPath, s.Token, map[string]string{"Content-Type": "text/plain"}, 415},
		{deletionPath, s.Token, map[string]string{"Host": "health.example"}, 421},
	} {
		if w := sendDeletion(router, "POST", c.path, c.token, valid, c.headers); w.Code != c.status {
			t.Fatal("request boundary", w.Code, c.status)
		}
	}
	for _, method := range []string{"GET", "DELETE", "PUT"} {
		if w := sendDeletion(router, method, deletionPath, "", valid, nil); w.Code != 401 {
			t.Fatal("unintended public method", method, w.Code)
		}
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_account_deletions`).Scan(&count); err != nil || count != 0 || !deletionAuthenticated(f, s.Token) {
		t.Fatal("invalid request changed account", count, err)
	}
}

func TestMySQLAccountDeletionHTTPGatesLimitsAndRollback(t *testing.T) {
	f := newPushFixture(t)
	s, _ := f.login(t, "owner", f.now)
	body := `{"requestID":"` + uuid.NewString() + `"}`
	limiter := &loginHTTPLimiter{}
	closed := deletionRouter(t, f, limiter, false)
	if w := sendDeletion(closed, "POST", deletionPath, s.Token, body, nil); w.Code != 503 || limiter.calls != 0 {
		t.Fatal("unconfigured endpoint enabled", w.Code)
	}
	router := deletionRouter(t, f, limiter, true)
	limiter.delay = 1500 * time.Millisecond
	if w := sendDeletion(router, "POST", deletionPath, s.Token, body, nil); w.Code != 429 || w.Header().Get("Retry-After") != "2" {
		t.Fatal("limit ignored", w.Code)
	}
	limiter.delay = 0
	limiter.err = errors.New("private Redis failure")
	if w := sendDeletion(router, "POST", deletionPath, s.Token, body, nil); w.Code != 503 || strings.Contains(w.Body.String(), "private") {
		t.Fatal("limiter failed open", w.Code)
	}
	limiter.err = nil
	if _, err := f.r.db.Exec(`ALTER TABLE album_deletion_ledger RENAME COLUMN request_id TO unavailable_request_id`); err != nil {
		t.Fatal(err)
	}
	if w := sendDeletion(router, "POST", deletionPath, s.Token, body, nil); w.Code != 503 || strings.Contains(w.Body.String(), "request_id") {
		t.Fatal("partial transaction accepted or leaked SQL", w.Code)
	}
	if !deletionAuthenticated(f, s.Token) {
		t.Fatal("failed request revoked access")
	}
	if _, err := f.r.db.Exec(`ALTER TABLE album_deletion_ledger RENAME COLUMN unavailable_request_id TO request_id`); err != nil {
		t.Fatal(err)
	}
	if w := sendDeletion(router, "POST", deletionPath, s.Token, body, nil); w.Code != 202 {
		t.Fatal("retry failed", w.Code)
	}
}

func TestGatewayAccountDeletionHTTPRemainsClosed(t *testing.T) {
	f := newAppleGatewayFixture(t)
	s, _, _ := f.login(t, "not-deleting")
	g, err := NewGateway(context.Background(), f.config, f.transport(t, func(*http.Request) (*http.Response, error) {
		t.Error("closed endpoint contacted Apple")
		return nil, ErrAppleCode
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	w := sendDeletion(g, "POST", deletionPath, s.Token, `{"requestID":"`+uuid.NewString()+`"}`, map[string]string{"Host": f.config.APIHost})
	if w.Code != 503 || !strings.Contains(w.Body.String(), "account_deletion_unavailable") {
		t.Fatal(w.Code, w.Body.String())
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_account_deletions`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
