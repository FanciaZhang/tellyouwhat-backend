package albums

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tellyouwhat/backend/internal/albumhttpapi"
)

func TestAlbumPushRealmSelectionAndHeader(t *testing.T) {
	for _, dual := range []bool{false, true} {
		r := &albumPushRegistrations{environment: "production", allowDevelopment: dual}
		h := &uploadHTTP{login: &LoginHTTPOptions{PushRegistrations: r}}
		for _, values := range [][]string{nil, {"production"}, {"development"}, {""}, {"Sandbox"}, {"development,production"}, {"production", "production"}, {"https://other.example"}} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/", nil)
			for _, value := range values {
				c.Request.Header.Add("X-Album-Push-Environment", value)
			}
			store, ok := h.pushRegistrationStore(c)
			want := len(values) == 0 || len(values) == 1 && (values[0] == "production" || dual && values[0] == "development")
			if ok != want {
				t.Fatalf("dual=%v values=%v accepted=%v", dual, values, ok)
			}
			if ok {
				expected := "production"
				if len(values) == 1 {
					expected = values[0]
				}
				if store.environment != expected || r.environment != "production" {
					t.Fatal("shared realm mutated")
				}
			} else if w.Code != 400 {
				t.Fatal("invalid realm did not fail closed")
			}
		}
	}
}

func TestAlbumDualPushConfigRequiresIsolatedDevelopmentIdentity(t *testing.T) {
	for _, value := range []string{"true", "false", "", "1", "yes"} {
		env := gatewayEnvironment(t)
		env["ALBUM_PUSH_ENVIRONMENT"] = "production"
		env["ALBUM_PUSH_ALLOW_DEVELOPMENT"] = value
		c, err := LoadGatewayConfig(func(k string) string { return env[k] })
		if value == "1" || value == "yes" {
			if err == nil {
				t.Fatal("invalid switch accepted")
			}
			continue
		}
		if err != nil || c.PushAllowDevelopment != (value == "true") {
			t.Fatal("valid switch", err)
		}
	}
	for _, base := range []string{"", "development"} {
		env := gatewayEnvironment(t)
		env["ALBUM_PUSH_ALLOW_DEVELOPMENT"] = "true"
		env["ALBUM_PUSH_ENVIRONMENT"] = base
		if _, err := LoadGatewayConfig(func(k string) string { return env[k] }); err == nil {
			t.Fatal("unsafe default accepted")
		}
	}
	env := gatewayEnvironment(t)
	env["ALBUM_DEPLOYMENT_ENVIRONMENT"] = "production"
	env["ALBUM_COS_BUCKET"] = albumProductionBucket
	env["ALBUM_DATABASE_DSN"] = "app:private-test-password@tcp(localhost:3306)/albums_production"
	env["ALBUM_PUSH_ALLOW_DEVELOPMENT"] = "true"
	env["ALBUM_PUSH_ENVIRONMENT"] = "production"
	if _, err := LoadGatewayConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("formal identity accepted development push")
	}
	env["ALBUM_PUSH_ALLOW_DEVELOPMENT"] = "false"
	env["ALBUM_PUSH_ENVIRONMENT"] = "development"
	if _, err := LoadGatewayConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("formal identity accepted a development default")
	}
}

func TestMySQLAlbumPushSwitchRealmFencesOldRequests(t *testing.T) {
	f := newPushFixture(t)
	f.r.allowDevelopment = true
	development, err := f.r.forEnvironment("development")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, _ := f.login(t, "switch-installation", f.now)
	oldRequest := pushUpdate("aabb", 0)
	old, err := development.update(ctx, s.Token, oldRequest, f.now)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.r.update(ctx, s.Token, pushUpdate("ccdd", 0), f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := development.target(ctx, s.AccountID, old.ID, old.Revision); err == nil {
		t.Fatal("old realm remained deliverable")
	}
	if _, err := development.update(ctx, s.Token, oldRequest, f.now); !errors.Is(err, errPushRegistrationConflict) {
		t.Fatal("old retry reactivated registration", err)
	}
	if _, err := f.r.target(ctx, s.AccountID, current.ID, current.Revision); err != nil {
		t.Fatal("new realm not deliverable", err)
	}
	if err := development.invalidate(ctx, current.ID, current.Revision, f.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.target(ctx, s.AccountID, current.ID, current.Revision); err != nil {
		t.Fatal("wrong realm 410 disabled registration", err)
	}
	previous, err := development.current(ctx, s.Token, f.now)
	if err != nil || previous.State != "disabled" || previous.Revision != old.Revision+1 {
		t.Fatal("switch did not fence revision", err)
	}
	var remaining int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_push_registrations WHERE registration_id=? AND (encrypted_token IS NOT NULL OR token_nonce IS NOT NULL)`, old.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("previous token retained", err)
	}
	// Opting out after installing the other signed variant also removes the old token.
	if _, err := development.update(ctx, s.Token, pushUpdate("", previous.Revision), f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.target(ctx, s.AccountID, current.ID, current.Revision); err == nil {
		t.Fatal("opt-out left opposite realm enabled")
	}
}

func TestMySQLRetentionDispatchBothRealmsWaitsForBothReceipts(t *testing.T) {
	f := newRetentionDispatchFixture(t, 2)
	f.push.r.allowDevelopment = true
	development, _ := f.push.r.forEnvironment("development")
	if _, err := development.update(context.Background(), f.sessions[1].Token, pushUpdate("abcd", 0), f.push.now); err != nil {
		t.Fatal(err)
	}
	productionCalls, developmentCalls := 0, 0
	retryAt := f.clock.Add(time.Hour)
	f.d.sender = retentionSendFunc(func(_ context.Context, token string, _ albumRetentionReminder, _ string) (albumAPNSResult, error) {
		productionCalls++
		if token != fmt.Sprintf("%064x", 1) {
			t.Error("wrong realm token sent to production")
		}
		return albumAPNSResult{Status: "accepted"}, nil
	})
	f.d.developmentSender = retentionSendFunc(func(_ context.Context, token string, _ albumRetentionReminder, _ string) (albumAPNSResult, error) {
		developmentCalls++
		if token != "abcd" {
			t.Error("wrong realm token sent to development")
		}
		if developmentCalls == 1 {
			return albumAPNSResult{Status: "retry", RetryAt: retryAt}, nil
		}
		return albumAPNSResult{Status: "accepted"}, nil
	})
	if found, err := f.d.dispatchOne(context.Background()); err != nil || !found {
		t.Fatal(found, err)
	}
	if f.reminder(t).State != "pending" || productionCalls != 1 || developmentCalls != 1 {
		t.Fatal("one realm prematurely settled both")
	}
	f.clock = retryAt
	if found, err := f.d.dispatchOne(context.Background()); err != nil || !found {
		t.Fatal(found, err)
	}
	if f.reminder(t).State != "delivered" || productionCalls != 1 || developmentCalls != 2 {
		t.Fatal("retry repeated accepted realm or omitted pending realm")
	}
}

func TestMySQLAlbumPushHTTPSelectsRealm(t *testing.T) {
	f := newPushFixture(t)
	f.r.allowDevelopment = true
	s, _ := f.login(t, "http-dual-realm", f.now)
	h := &uploadHTTP{login: &LoginHTTPOptions{PushRegistrations: f.r, PushLimiter: &loginHTTPLimiter{}}}
	router := gin.New()
	albumhttpapi.RegisterHandlers(router, h)
	for _, realm := range []string{"development", "production"} {
		raw, _ := json.Marshal(pushUpdate("abcd", 0))
		w := httptest.NewRecorder()
		request := httptest.NewRequest("PUT", "/v1/albums/notifications/registration", strings.NewReader(string(raw)))
		request.Header.Set("Authorization", "Bearer "+s.Token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Album-Push-Environment", realm)
		router.ServeHTTP(w, request)
		if w.Code != 200 {
			t.Fatal("realm update", realm, w.Code)
		}
		store, _ := f.r.forEnvironment(realm)
		current, err := store.current(context.Background(), s.Token, f.now)
		if err != nil || current.State != "enabled" || current.Revision != 1 {
			t.Fatal("HTTP registered wrong realm", err)
		}
		request = httptest.NewRequest("GET", "/v1/albums/notifications/registration", nil)
		request.Header.Set("Authorization", "Bearer "+s.Token)
		request.Header.Set("X-Album-Push-Environment", realm)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, request)
		var read albumPushRegistration
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &read) != nil || read != current {
			t.Fatal("generated GET selected wrong registration")
		}
	}
}

func TestMySQLRetentionDispatchRealmSwitchAfterAcceptance(t *testing.T) {
	f := newRetentionDispatchFixture(t, 2)
	f.push.r.allowDevelopment = true
	calls := map[string]int{}
	retryAt := f.clock.Add(time.Hour)
	f.d.sender = retentionSendFunc(func(_ context.Context, token string, _ albumRetentionReminder, _ string) (albumAPNSResult, error) {
		calls[token]++
		if token == fmt.Sprintf("%064x", 2) && calls[token] == 1 {
			return albumAPNSResult{Status: "retry", RetryAt: retryAt}, nil
		}
		return albumAPNSResult{Status: "accepted"}, nil
	})
	f.d.developmentSender = retentionSendFunc(func(context.Context, string, albumRetentionReminder, string) (albumAPNSResult, error) {
		t.Error("re-signing an installation repeated its accepted reminder")
		return albumAPNSResult{Status: "accepted"}, nil
	})
	if found, err := f.d.dispatchOne(context.Background()); err != nil || !found {
		t.Fatal(found, err)
	}
	development, _ := f.push.r.forEnvironment("development")
	if _, err := development.update(context.Background(), f.sessions[0].Token, pushUpdate("abcd", 0), f.push.now); err != nil {
		t.Fatal(err)
	}
	f.clock = retryAt
	if found, err := f.d.dispatchOne(context.Background()); err != nil || !found {
		t.Fatal(found, err)
	}
	if calls[fmt.Sprintf("%064x", 1)] != 1 || calls[fmt.Sprintf("%064x", 2)] != 2 || f.reminder(t).State != "delivered" {
		t.Fatal("accepted session was not deduplicated")
	}
}
