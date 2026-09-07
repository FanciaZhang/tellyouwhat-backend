package development

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/journal/voice"
)

type model struct{ calls int }

func (m *model) Organize(context.Context, contracts.OrganizeRequest, bool) (provider.Result, error) {
	m.calls++
	return provider.Result{}, nil
}

func TestPrivateDevelopmentBoundary(t *testing.T) {
	now := time.Now()
	token := strings.Repeat("a", 43)
	m := &model{}
	c := Config{Token: token, ExpiresAt: now.Add(time.Hour), Now: func() time.Time { return now }, Organizer: m, Speech: voice.ASR{}, Rewriter: voice.ArkRewriter{}}
	h, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, auth, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Tellyouwhat-Request-ID", "19be2f9e-bd92-4699-b561-e3816092114c")
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, auth := range []string{"", strings.Repeat("b", 43)} {
		if w := request("GET", "/v1/ai/quota", auth, ""); w.Code != 401 {
			t.Fatalf("unauthorized: %d %s", w.Code, w.Body)
		}
	}
	for _, path := range []string{"/v1/entitlements/transactions", "/v1/dev/entitlements/activate", "/v1/ai/requests", "/v1/attestation/keys"} {
		if w := request("POST", path, token, "{}"); w.Code != 404 {
			t.Fatalf("unexpected exposed route %s: %d", path, w.Code)
		}
	}
	if w := request("GET", "/v1/ai/quota", token, ""); w.Code != 200 {
		t.Fatalf("quota: %d %s", w.Code, w.Body)
	}
	body := `{"sessionID":"19be2f9e-bd92-4699-b561-e3816092114c","consentVersion":"journal-voice-v1"}`
	if w := request("POST", "/v1/journal/voice/sessions", token, body); w.Code != 403 {
		t.Fatalf("missing consent: %d %s", w.Code, w.Body)
	}
	consent := `{"consents":[{"scope":"managed_subscription","documentVersion":"2026-08-24","granted":true}]}`
	if w := request("POST", "/v1/privacy/consents", token, consent); w.Code != 200 {
		t.Fatalf("consent: %d %s", w.Code, w.Body)
	}
	if w := request("POST", "/v1/journal/voice/sessions", token, body); w.Code != 201 {
		t.Fatalf("voice ticket: %d %s", w.Code, w.Body)
	}
	// A developer token must never replace a one-time voice ticket.
	if w := request("GET", "/v1/journal/voice/sessions/19be2f9e-bd92-4699-b561-e3816092114c/stream", token, ""); w.Code != 401 {
		t.Fatalf("invalid stream ticket: %d %s", w.Code, w.Body)
	}
	now = now.Add(2 * time.Hour)
	if w := request("GET", "/v1/ai/quota", token, ""); w.Code != 401 {
		t.Fatalf("expired session: %d", w.Code)
	}
	if m.calls != 0 {
		t.Fatal("boundary probes must not invoke a provider")
	}
}

func TestRejectsUnboundedDevelopmentSessions(t *testing.T) {
	now := time.Now()
	for _, duration := range []time.Duration{-time.Second, 25 * time.Hour} {
		if _, err := New(Config{Token: strings.Repeat("a", 43), ExpiresAt: now.Add(duration), Organizer: &model{}, Speech: voice.ASR{}, Rewriter: voice.ArkRewriter{}}); err == nil {
			t.Fatal("invalid expiry accepted")
		}
	}
	if _, err := New(Config{Token: "weak", ExpiresAt: now.Add(time.Hour), Organizer: &model{}, Speech: voice.ASR{}, Rewriter: voice.ArkRewriter{}}); err == nil {
		t.Fatal("short token accepted")
	}
}
