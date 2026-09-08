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
	c := Config{Token: token, Now: func() time.Time { return now }, Organizer: m, Speech: voice.ASR{}, Rewriter: voice.ArkRewriter{}}
	h, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	mode := "forced"
	request := func(method, path, auth, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Tellyouwhat-Request-ID", "19be2f9e-bd92-4699-b561-e3816092114c")
		r.Header.Set("X-Journal-Development-Installation", "19be2f9e-bd92-4699-b561-e3816092114c")
		r.Header.Set("X-Journal-Development-Mode", mode)
		r.Header.Set("X-Journal-Development-Started-At", "2026-01-01T00:00:00Z")
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
	now = now.Add(90 * 24 * time.Hour)
	if w := request("GET", "/v1/ai/quota", token, ""); w.Code != 200 {
		t.Fatalf("forced simulation incorrectly expired: %d", w.Code)
	}
	mode = "monthly"
	if w := request("POST", "/v1/journal/voice/sessions", token, body); w.Code != 403 || !strings.Contains(w.Body.String(), "managed_subscription_required") {
		t.Fatalf("expired monthly simulation authorized voice: %d %s", w.Code, w.Body)
	}
	mode = "forced"
	h, err = New(c) // A service restart must not expire the provisioned credential.
	if err != nil {
		t.Fatal(err)
	}
	if w := request("POST", "/v1/privacy/consents", token, consent); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := request("POST", "/v1/journal/voice/sessions", token, body); w.Code != 201 {
		t.Fatal(w.Body)
	}
	if m.calls != 0 {
		t.Fatal("boundary probes must not invoke a provider")
	}
}

func TestRejectsWeakDevelopmentCredential(t *testing.T) {
	if _, err := New(Config{Token: "weak", Organizer: &model{}, Speech: voice.ASR{}, Rewriter: voice.ArkRewriter{}}); err == nil {
		t.Fatal("short token accepted")
	}
}

func TestMonthlySimulationUsesOriginalCalendarMonth(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/ai/quota", nil)
	r.Header.Set("X-Journal-Development-Installation", "19be2f9e-bd92-4699-b561-e3816092114c")
	r.Header.Set("X-Journal-Development-Mode", "monthly")
	r.Header.Set("X-Journal-Development-Started-At", "2028-01-31T12:30:00Z")
	now := time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC)
	record, err := simulatedRecord(r, now)
	if err != nil {
		t.Fatal(err)
	}
	expected := time.Date(2028, 2, 29, 12, 30, 0, 0, time.UTC)
	if record.ExpiresAt != expected {
		t.Fatalf("calendar month: %v", record.ExpiresAt)
	}
	reopened, err := simulatedRecord(r, now.Add(29*24*time.Hour))
	if err != nil || reopened.ExpiresAt != expected || reopened.ExpiresAt.After(now.Add(29*24*time.Hour)) {
		t.Fatal("reopening renewed an expired month")
	}
	r.Header.Set("X-Journal-Development-Mode", "expired")
	expired, err := simulatedRecord(r, now)
	if err != nil || expired.ExpiresAt.After(now) {
		t.Fatal("expired mock granted access")
	}
	r.Header.Set("X-Journal-Development-Mode", "storeKit")
	if _, err = simulatedRecord(r, now); err == nil {
		t.Fatal("real purchase path accepted as development simulation")
	}
}
