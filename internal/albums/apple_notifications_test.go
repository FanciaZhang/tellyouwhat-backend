package albums

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func accountEventClaims(now time.Time) jwt.MapClaims {
	return jwt.MapClaims{"iss": appleIssuer, "aud": albumDevelopmentAudience, "iat": now.Unix(), "jti": "event-id", "events": map[string]any{"type": "consent-revoked", "sub": "apple-user", "event_time": now.Add(-time.Second).Unix()}}
}
func TestAppleAccountNotificationSignaturePurposeAndFields(t *testing.T) {
	key := testAppleKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	v := NewAppleIdentityVerifier(albumDevelopmentAudience, appleTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != appleIssuer+"/auth/keys" {
			t.Fatal("untrusted key URL")
		}
		return appleResponse(testAppleJWKS(key, "key")), nil
	}))
	v.now = func() time.Time { return now }
	for _, encodedEvents := range []bool{false, true} {
		claims := accountEventClaims(now)
		if encodedEvents {
			data, _ := json.Marshal(claims["events"])
			claims["events"] = string(data)
		}
		got, err := v.verifyAccountEvent(context.Background(), appleSigned(t, key, "key", claims))
		if err != nil || got.Subject != "apple-user" || got.Type != "consent-revoked" {
			t.Fatal("valid notification", err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(jwt.MapClaims)
	}{
		{"audience", func(c jwt.MapClaims) { c["aud"] = "cn.tellyouwhat.health" }},
		{"multiple audience", func(c jwt.MapClaims) { c["aud"] = []string{albumDevelopmentAudience, "other"} }},
		{"issuer", func(c jwt.MapClaims) { c["iss"] = "https://attacker.invalid" }},
		{"missing iat", func(c jwt.MapClaims) { delete(c, "iat") }},
		{"future iat", func(c jwt.MapClaims) { c["iat"] = now.Add(time.Minute).Unix() }},
		{"missing jti", func(c jwt.MapClaims) { delete(c, "jti") }},
		{"expired", func(c jwt.MapClaims) { c["exp"] = now.Add(-time.Second).Unix() }},
		{"missing event", func(c jwt.MapClaims) { delete(c, "events") }},
		{"unknown type", func(c jwt.MapClaims) { c["events"].(map[string]any)["type"] = "unknown" }},
		{"missing subject", func(c jwt.MapClaims) { delete(c["events"].(map[string]any), "sub") }},
		{"missing time", func(c jwt.MapClaims) { delete(c["events"].(map[string]any), "event_time") }},
		{"future event", func(c jwt.MapClaims) { c["events"].(map[string]any)["event_time"] = now.Add(time.Minute).Unix() }},
		{"millisecond units", func(c jwt.MapClaims) { c["events"].(map[string]any)["event_time"] = now.UnixMilli() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := accountEventClaims(now)
			tc.edit(c)
			if _, err := v.verifyAccountEvent(context.Background(), appleSigned(t, key, "key", c)); !errors.Is(err, errAppleNotification) {
				t.Fatal("invalid notification accepted", err)
			}
		})
	}
	if _, err := v.verifyAccountEvent(context.Background(), appleSigned(t, testAppleKey(t), "key", accountEventClaims(now))); !errors.Is(err, errAppleNotification) {
		t.Fatal("forged signature", err)
	}
	if _, err := v.verifyAccountEvent(context.Background(), appleSigned(t, key, "key", appleClaims(now))); !errors.Is(err, errAppleNotification) {
		t.Fatal("identity token used as notification", err)
	}
	unreachable := NewAppleIdentityVerifier(albumDevelopmentAudience, appleTestTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("private upstream diagnostic") }))
	if _, err := unreachable.verifyAccountEvent(context.Background(), appleSigned(t, key, "key", accountEventClaims(now))); !errors.Is(err, errAppleKeyUnavailable) {
		t.Fatal("transient failure must be retryable", err)
	}
}

func eventLogin(t *testing.T, f *pushFixture, subject string, iat time.Time) (AlbumSession, AccountLogin, AppleGrant) {
	t.Helper()
	grant := AppleGrant{Identity: AppleIdentity{Audience: albumDevelopmentAudience, Subject: subject, IssuedAt: iat}, RefreshToken: uuid.NewString()}
	login, err := f.accounts.RegisterAppleLogin(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.sessions.Issue(context.Background(), login)
	if err != nil {
		t.Fatal(err)
	}
	return session, login, grant
}
func sessionAccepted(f *pushFixture, s AlbumSession) bool {
	r := httptest.NewRequest("GET", "https://albums.example/v1/albums/account", nil)
	r.Header.Set("Authorization", "Bearer "+s.Token)
	_, err := f.sessions.Authenticate(context.Background(), r)
	return err == nil
}
func TestMySQLAppleEventRevokesOldDevicesAndPushButPreservesNewLogin(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	at := f.now.Add(-10 * time.Second).Truncate(time.Second)
	a, login, oldGrant := eventLogin(t, f, "owner", at.Add(-time.Second))
	b, _, _ := eventLogin(t, f, "owner", at.Add(-time.Second))
	fresh, _, _ := eventLogin(t, f, "owner", at.Add(time.Second))
	foreign, _, _ := eventLogin(t, f, "foreign", at.Add(-time.Second))
	p, err := f.r.update(ctx, a.Token, pushUpdate(strings.Repeat("ab", 32), 0), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.r.db.Exec(`UPDATE album_storage_accounts SET quota_bytes=12345 WHERE owner_id=?`, login.AccountID); err != nil {
		t.Fatal(err)
	}
	event := appleAccountEvent{Audience: albumDevelopmentAudience, Type: "consent-revoked", Subject: "owner", EventTime: at.Unix()}
	if err = f.accounts.applyAppleEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if sessionAccepted(f, a) || sessionAccepted(f, b) || !sessionAccepted(f, fresh) || !sessionAccepted(f, foreign) {
		t.Fatal("wrong session generation revoked")
	}
	if _, err = f.r.target(ctx, login.AccountID, p.ID, p.Revision); err == nil {
		t.Fatal("revoked grant still sends push")
	}
	reopened, _ := NewMySQLAccounts(albumDevelopmentAudience, f.r.db, f.r.cipher)
	if err = reopened.applyAppleEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	event.EventTime--
	if err = reopened.applyAppleEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if !sessionAccepted(f, fresh) {
		t.Fatal("delayed duplicate revoked fresh login")
	}
	if _, err = f.accounts.RegisterAppleLogin(ctx, oldGrant); !errors.Is(err, ErrAppleCode) {
		t.Fatal("old proof reauthorized", err)
	}
	oldGrant.Identity.IssuedAt = at.Add(time.Second)
	if _, err = f.accounts.RegisterAppleLogin(ctx, oldGrant); !errors.Is(err, ErrAppleCode) {
		t.Fatal("revoked refresh token revived", err)
	}
	after, _, _ := eventLogin(t, f, "owner", at.Add(2*time.Second))
	if !sessionAccepted(f, after) {
		t.Fatal("new consent rejected")
	}
	var quota int64
	if err = f.r.db.QueryRow(`SELECT quota_bytes FROM album_storage_accounts WHERE owner_id=?`, login.AccountID).Scan(&quota); err != nil || quota != 12345 {
		t.Fatal("quota changed", err)
	}
	event.Type = "account-deleted"
	if err = f.accounts.applyAppleEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if sessionAccepted(f, fresh) || sessionAccepted(f, after) || !sessionAccepted(f, foreign) {
		t.Fatal("account deletion access scope")
	}
	oldGrant.RefreshToken = uuid.NewString()
	oldGrant.Identity.IssuedAt = f.now
	if _, err = f.accounts.RegisterAppleLogin(ctx, oldGrant); !errors.Is(err, ErrAppleCode) {
		t.Fatal("deleted identity recreated", err)
	}
}
func TestMySQLAppleEventBeforeFirstLoginAndRollback(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	at := f.now.Add(-time.Minute).Truncate(time.Second)
	event := appleAccountEvent{Audience: albumDevelopmentAudience, Type: "consent-revoked", Subject: "not-yet-registered", EventTime: at.Unix()}
	if err := f.accounts.applyAppleEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	stale := AppleGrant{Identity: AppleIdentity{Audience: albumDevelopmentAudience, Subject: event.Subject, IssuedAt: at}, RefreshToken: uuid.NewString()}
	if _, err := f.accounts.RegisterAppleLogin(ctx, stale); !errors.Is(err, ErrAppleCode) {
		t.Fatal("late stale login accepted", err)
	}
	s, login, _ := eventLogin(t, f, event.Subject, at.Add(time.Second))
	if _, err := f.r.db.Exec(`ALTER TABLE album_apple_grants RENAME COLUMN revoked_at TO unavailable_revoked_at`); err != nil {
		t.Fatal(err)
	}
	event.EventTime = at.Add(2 * time.Second).Unix()
	if err := f.accounts.applyAppleEvent(ctx, event); err == nil {
		t.Fatal("failed revoke acknowledged")
	}
	if _, err := f.r.db.Exec(`ALTER TABLE album_apple_grants RENAME COLUMN unavailable_revoked_at TO revoked_at`); err != nil {
		t.Fatal(err)
	}
	if !sessionAccepted(f, s) {
		t.Fatal("rolled-back event partly revoked session")
	}
	var before time.Time
	if err := f.r.db.QueryRow(`SELECT revoked_before FROM album_apple_identity_state WHERE subject_sha256=?`, appleSubjectHash(albumDevelopmentAudience, event.Subject)).Scan(&before); err != nil || !before.Equal(at) {
		t.Fatal("fence advanced without grant commit", before, err)
	}
	if err := f.accounts.applyAppleEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if sessionAccepted(f, s) {
		t.Fatal("retry failed to revoke")
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_storage_accounts WHERE owner_id=?`, login.AccountID).Scan(&count); err != nil || count != 1 {
		t.Fatal("media owner removed")
	}
}
func TestMySQLAppleNotificationRacesWithStaleLogin(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	at := f.now.Add(-time.Minute).Truncate(time.Second)
	_, _, _ = eventLogin(t, f, "racing", at.Add(-time.Second))
	var wg sync.WaitGroup
	issued := make(chan AlbumSession, 12)
	failures := make(chan error, 12)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			login, err := f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Audience: albumDevelopmentAudience, Subject: "racing", IssuedAt: at.Add(-time.Second)}, RefreshToken: uuid.NewString()})
			if errors.Is(err, ErrAppleCode) {
				return
			}
			if err != nil {
				failures <- err
				return
			}
			s, err := f.sessions.Issue(ctx, login)
			if errors.Is(err, ErrSession) {
				return
			}
			if err != nil {
				failures <- err
				return
			}
			issued <- s
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := f.accounts.applyAppleEvent(ctx, appleAccountEvent{Audience: albumDevelopmentAudience, Type: "consent-revoked", Subject: "racing", EventTime: at.Unix()}); err != nil {
			failures <- err
		}
	}()
	wg.Wait()
	close(issued)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	for s := range issued {
		if sessionAccepted(f, s) {
			t.Error("racing stale session survived")
		}
	}
}

func TestMySQLAppleNotificationHTTPBoundariesAndCommittedReceipt(t *testing.T) {
	f := newPushFixture(t)
	key := testAppleKey(t)
	now := f.now.Truncate(time.Second)
	old, _, _ := eventLogin(t, f, "apple-user", now.Add(-time.Minute))
	unavailable := false
	verifier := NewAppleIdentityVerifier(albumDevelopmentAudience, appleTestTransport(func(*http.Request) (*http.Response, error) {
		if unavailable {
			return nil, errors.New("private provider detail")
		}
		return appleResponse(testAppleJWKS(key, "key")), nil
	}))
	verifier.now = func() time.Time { return now }
	uploads, _, _, _, _ := uploadFixture(t)
	pipeline := &loginPipelineFixture{}
	challenges, _ := NewLoginChallengeService(&challengeTestStore{rows: map[string]StoredLoginChallenge{}, used: map[string]bool{}}, time.Now)
	login, _ := NewLoginService(challenges, pipeline, pipeline, pipeline)
	limiter := &loginHTTPLimiter{}
	router, err := NewHTTPRouterWithLogin("albums.example", testAccountAuth{err: ErrSession}, uploads, LoginHTTPOptions{Service: login, Limiter: limiter, Sessions: &loginHTTPRevoker{}, AppleNotifications: &appleAccountNotifications{verifier, f.accounts}})
	if err != nil {
		t.Fatal(err)
	}
	bodyFor := func(payload string) string {
		data, _ := json.Marshal(map[string]string{"payload": payload})
		return string(data)
	}
	send := func(method, path, body, contentType string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://albums.example"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("X-Forwarded-For", "198.51.100.5")
		r.RemoteAddr = "192.0.2.5:1234"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	path := "/v1/albums/auth/apple/notifications"
	valid := bodyFor(appleSigned(t, key, "key", accountEventClaims(now)))
	for _, tc := range []struct {
		path, body, media string
		status            int
	}{
		{path, `{}`, "application/json", 400}, {path, valid + ` {}`, "application/json", 400},
		{path, `{"payload":"x","owner":"injected"}`, "application/json", 400},
		{path, bodyFor(strings.Repeat("x", (32<<10)+1)), "application/json", 400},
		{path, valid, "text/plain", 415}, {path + "?payload=x", valid, "application/json", 400},
		{path, bodyFor("not-a-jwt"), "application/json", 422},
	} {
		if w := send("POST", tc.path, tc.body, tc.media); w.Code != tc.status {
			t.Fatalf("boundary expected %d got %d", tc.status, w.Code)
		}
	}
	limiter.delay = time.Second
	if w := send("POST", path, valid, "application/json"); w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("rate limit", w.Code)
	}
	limiter.delay = 0
	limiter.err = errors.New("private Redis error")
	if w := send("POST", path, valid, "application/json"); w.Code != 503 || strings.Contains(w.Body.String(), "private") {
		t.Fatal("limiter fail closed")
	}
	limiter.err = nil
	unavailable = true
	if w := send("POST", path, valid, "application/json"); w.Code != 503 {
		t.Fatal("key service failure must retry", w.Code)
	}
	if !sessionAccepted(f, old) {
		t.Fatal("failed verification mutated grant")
	}
	unavailable = false
	verifier.lastFetch = time.Time{}
	if w := send("POST", path, valid, "application/json; charset=utf-8"); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("verified event not accepted", w.Code)
	}
	if limiter.operation != "apple-notification" || limiter.address != "192.0.2.5" {
		t.Fatal("wrong limiter scope/proxy trust")
	}
	if sessionAccepted(f, old) {
		t.Fatal("acknowledged before revocation")
	}
	if w := send("POST", path, valid, "application/json"); w.Code != 200 {
		t.Fatal("idempotent retry", w.Code)
	}
	if w := send("GET", path, "", "application/json"); w.Code != 401 {
		t.Fatal("auth bypass too broad", w.Code)
	}
}
