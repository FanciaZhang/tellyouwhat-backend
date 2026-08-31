package adminportal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/appstoreconnect"
)

func TestReadinessChecksDependenciesWhileHealthRemainsLive(t *testing.T) {
	t.Parallel()
	server := &Server{readiness: func(context.Context) error { return errors.New("database unavailable") }}
	ready := httptest.NewRecorder()
	server.ready(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness ignored dependency failure: %d %s", ready.Code, ready.Body.String())
	}
	health := httptest.NewRecorder()
	server.health(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("liveness unexpectedly failed: %d %s", health.Code, health.Body.String())
	}
}

func TestPreviewTokenBindsDraftAndExpires(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	server := &Server{now: func() time.Time { return now }, config: Config{PreviewSigningKey: []byte("01234567890123456789012345678901")}}
	draft := appstoreconnect.OfferDraft{Name: "朋友体验", Duration: "ONE_MONTH", CustomerEligibilities: []string{"NEW"}}
	token, _, err := server.signPreview("health", draft)
	if err != nil || !server.verifyPreview("health", draft, token) {
		t.Fatalf("valid token failed: %v", err)
	}
	changed := draft
	changed.Duration = "ONE_YEAR"
	if server.verifyPreview("health", changed, token) {
		t.Fatal("token accepted a changed draft")
	}
	if server.verifyPreview("journal", draft, token) {
		t.Fatal("token crossed application boundary")
	}
	now = now.Add(10 * time.Minute)
	if server.verifyPreview("health", draft, token) {
		t.Fatal("expired token was accepted")
	}
}

func TestAuthenticationRateLimitIsPerAddressAndResets(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	limiter := newRateLimiter(func() time.Time { return now })
	request := httptest.NewRequest("POST", "/api/v1/auth/login/options", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	for range 30 {
		if !limiter.allow(request) {
			t.Fatal("request was limited too early")
		}
	}
	if limiter.allow(request) {
		t.Fatal("31st authentication request was allowed")
	}
	now = now.Add(time.Minute)
	if !limiter.allow(request) {
		t.Fatal("rate limit did not reset")
	}
}

func TestOfferDraftPolicy(t *testing.T) {
	valid := appstoreconnect.OfferDraft{Name: "朋友体验", Duration: "ONE_YEAR", CustomerEligibilities: []string{"NEW", "EXPIRED"}}
	if !draftIsValid(valid) {
		t.Fatal("valid draft rejected")
	}
	invalid := valid
	invalid.Duration = "PERMANENT"
	if draftIsValid(invalid) {
		t.Fatal("unsupported permanent duration accepted")
	}
}

func TestResourceIDRejectsHeaderAndPathCharacters(t *testing.T) {
	for _, value := range []string{"batch-1", "offer_2:production"} {
		if cleanID(value) != value {
			t.Fatalf("valid resource ID rejected: %q", value)
		}
	}
	for _, value := range []string{"../batch", "batch/1", "batch\nContent-Type:text/html", `batch".csv`} {
		if cleanID(value) != "" {
			t.Fatalf("unsafe resource ID accepted: %q", value)
		}
	}
}
