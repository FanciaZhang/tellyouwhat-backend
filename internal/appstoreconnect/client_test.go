package appstoreconnect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestListOffersPaginatesAndUsesScopedJWT(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		if request.URL.Path != "/v1/subscriptions/subscription-1/offerCodes" {
			t.Errorf("unexpected path %q", request.URL.Path)
		}
		claims := decodeClaims(t, strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		scope, ok := claims["scope"].([]any)
		if !ok || len(scope) != 1 || scope[0] != "GET /v1/subscriptions/subscription-1/offerCodes" {
			t.Errorf("unexpected JWT scope: %#v", claims["scope"])
		}
		writer.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			_, _ = writer.Write([]byte(`{"data":[{"type":"subscriptionOfferCodes","id":"offer-1","attributes":{"name":"朋友体验","duration":"ONE_MONTH","offerMode":"FREE_TRIAL","numberOfPeriods":1,"active":true}}],"links":{"next":"` + serverURL(request) + `/v1/subscriptions/subscription-1/offerCodes?page=2"}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"data":[{"type":"subscriptionOfferCodes","id":"offer-2","attributes":{"name":"老友续期","duration":"TWO_MONTHS","offerMode":"FREE_TRIAL","numberOfPeriods":1,"active":false}}],"links":{}}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{
		BaseURL: server.URL, IssuerID: "issuer", KeyID: "key", SubscriptionID: "subscription-1",
		SigningKey: key, Now: func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	offers, err := client.ListOffers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 2 || offers[0].ID != "offer-1" || offers[1].Duration != "TWO_MONTHS" || requestCount != 2 {
		t.Fatalf("unexpected offers: %#v (%d requests)", offers, requestCount)
	}
}

func TestListOffersRejectsPaginationToAnotherHost(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"data":[],"links":{"next":"https://attacker.invalid/v1/subscriptions/subscription-1/offerCodes"}}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{
		BaseURL: server.URL, IssuerID: "issuer", KeyID: "key", SubscriptionID: "subscription-1", SigningKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListOffers(context.Background()); err != ErrInvalid {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
}

func TestListOffersMapsForbidden(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	client, _ := NewClient(Config{
		BaseURL: server.URL, IssuerID: "issuer", KeyID: "key", SubscriptionID: "subscription-1", SigningKey: key,
	})
	if _, err := client.ListOffers(context.Background()); err != ErrForbidden {
		t.Fatalf("got %v, want ErrForbidden", err)
	}
}

func TestCreateFreeOfferUsesConstrainedApplePayload(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/subscriptionOfferCodes" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		claims := decodeClaims(t, strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		if scope := claims["scope"].([]any); len(scope) != 1 || scope[0] != "POST /v1/subscriptionOfferCodes" {
			t.Fatalf("unexpected scope %#v", scope)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		attributes := body["data"].(map[string]any)["attributes"].(map[string]any)
		if attributes["offerMode"] != "FREE_TRIAL" || attributes["numberOfPeriods"] != float64(1) ||
			attributes["offerEligibility"] != "REPLACE_INTRO_OFFERS" || attributes["targetSubscriptionPlanType"] != "MONTHLY" {
			t.Fatalf("unconstrained offer attributes: %#v", attributes)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"data":{"type":"subscriptionOfferCodes","id":"created","attributes":{"name":"朋友体验","duration":"ONE_MONTH","offerMode":"FREE_TRIAL","numberOfPeriods":1,"active":true}}}`))
	}))
	defer server.Close()
	client, _ := NewClient(Config{BaseURL: server.URL, IssuerID: "issuer", KeyID: "key", SubscriptionID: "monthly", SigningKey: key})
	offer, err := client.CreateFreeOffer(context.Background(), OfferDraft{Name: "朋友体验", Duration: "ONE_MONTH", CustomerEligibilities: []string{"NEW"}})
	if err != nil || offer.ID != "created" {
		t.Fatalf("offer = %#v, err = %v", offer, err)
	}
}

func TestDownloadOneTimeCodesReturnsCSVWithNarrowScope(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		claims := decodeClaims(t, strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		scope := claims["scope"].([]any)
		if len(scope) != 1 || scope[0] != "GET /v1/subscriptionOfferCodeOneTimeUseCodes/batch-1/values" || request.Header.Get("Accept") != "text/csv" {
			t.Fatalf("unexpected request scope or accept: %#v %q", scope, request.Header.Get("Accept"))
		}
		writer.Header().Set("Content-Type", "text/csv")
		_, _ = writer.Write([]byte("code\nABC123\n"))
	}))
	defer server.Close()
	client, _ := NewClient(Config{BaseURL: server.URL, IssuerID: "issuer", KeyID: "key", SubscriptionID: "monthly", SigningKey: key})
	data, err := client.DownloadOneTimeCodes(context.Background(), "batch-1")
	if err != nil || string(data) != "code\nABC123\n" {
		t.Fatalf("data = %q, err = %v", data, err)
	}
}

func decodeClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("invalid JWT %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}
