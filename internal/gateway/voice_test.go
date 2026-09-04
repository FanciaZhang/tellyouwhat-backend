package gateway

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"golang.org/x/net/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVoiceAdmissionRequiresOwnSubscriptionAndExplicitConsent(t *testing.T) {
	for _, tt := range []struct {
		name                                       string
		authenticated, paid, consent, voiceConsent bool
		status                                     int
	}{{"no assertion", false, true, true, true, 401}, {"unpaid", true, false, true, true, 403}, {"privacy", true, true, false, true, 403}, {"voice consent", true, true, true, false, 422}, {"paid", true, true, true, true, 201}} {
		t.Run(tt.name, func(t *testing.T) {
			store := entitlement.NewMemoryStore()
			store.Upsert(context.Background(), entitlement.Record{KeyID: "key", TransactionID: "original", Environment: "sandbox", StartedAt: time.Now().AddDate(0, -1, 0), ExpiresAt: time.Now().Add(time.Hour)})
			s := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: "journal"}, Entitlements: fakeEntitlements{allowed: tt.paid}, Consent: fakeConsentGate{granted: tt.consent}, RequiredConsentScopes: []string{"managed_subscription"}, Voice: &voice.Service{Store: voice.NewMemoryStore(), Secret: make([]byte, 32)}, VoiceEntitlements: store})
			version := ""
			if tt.voiceConsent {
				version = voice.Version
			}
			body, _ := json.Marshal(map[string]string{"sessionID": uuid.NewString(), "consentVersion": version})
			request := httptest.NewRequest(http.MethodPost, "/v1/journal/voice/sessions", strings.NewReader(string(body)))
			request.Header.Set("X-Tellyouwhat-Request-ID", "19be2f9e-bd92-4699-b561-e3816092114c")
			if tt.authenticated {
				request.Header.Set("X-Tellyouwhat-Key-ID", "key")
				request.Header.Set("X-Tellyouwhat-Assertion", "assertion")
				request.Header.Set("X-Tellyouwhat-Nonce", "nonce")
			}
			response := httptest.NewRecorder()
			s.Router().ServeHTTP(response, request)
			if response.Code != tt.status {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestGeneratedVoiceStreamUpgradesAndRejectsTicketReplay(t *testing.T) {
	service := &voice.Service{Store: voice.NewMemoryStore(), Secret: make([]byte, 32)}
	sessionID := uuid.NewString()
	ticket, err := service.Issue(context.Background(), voice.Identity{Owner: "subscriber", KeyID: "key", Anchor: time.Now().AddDate(0, -1, 0), ExpiresAt: time.Now().Add(time.Hour)}, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Voice: service}).Router())
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/journal/voice/sessions/" + sessionID + "/stream"
	config, err := websocket.NewConfig(endpoint, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.Header.Set("Authorization", "Bearer "+ticket.Token)
	connection, err := config.DialContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	var event voice.Event
	if err := websocket.JSON.Receive(connection, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "ready" {
		t.Fatalf("unexpected initial event: %s", event.Type)
	}
	if replay, err := config.DialContext(context.Background()); err == nil {
		replay.Close()
		t.Fatal("a consumed ticket must not upgrade another connection")
	}
}
