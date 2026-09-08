package appstoreconnect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

func TestCreateFreeOfferUsesAvailableTerritoriesAndPreservesRenewal(t *testing.T) {
	for _, renew := range []bool{false, true} {
		t.Run(fmt.Sprint(renew), func(t *testing.T) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				claims := decodeClaims(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				if r.Method == http.MethodGet {
					scope := claims["scope"].([]any)
					if len(scope) != 1 || scope[0] != "GET "+r.URL.Path {
						t.Errorf("unexpected read scope: %v", scope)
					}
					switch r.URL.Path {
					case "/v1/subscriptions/monthly/planAvailabilities":
						fmt.Fprint(w, `{"data":[{"type":"subscriptionPlanAvailabilities","id":"annual-commitment","attributes":{"planType":"MONTHLY"}},{"type":"subscriptionPlanAvailabilities","id":"standard","attributes":{"planType":"UPFRONT"}}]}`)
					case "/v1/subscriptionPlanAvailabilities/standard/availableTerritories":
						if r.URL.Query().Get("page") == "2" {
							fmt.Fprint(w, `{"data":[{"type":"territories","id":"USA"}]}`)
						} else {
							fmt.Fprintf(w, `{"data":[{"type":"territories","id":"CHN"}],"links":{"next":%q}}`, serverURL(r)+r.URL.Path+"?page=2")
						}
					default:
						t.Errorf("unexpected GET %s", r.URL.Path)
					}
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/subscriptionOfferCodes" {
					t.Errorf("unexpected write %s %s", r.Method, r.URL.Path)
				}
				if _, exists := claims["scope"]; exists {
					t.Error("write token contains unsupported scope claim")
				}
				if claims["exp"].(float64)-claims["iat"].(float64) != 300 {
					t.Error("write token lifetime is not five minutes")
				}
				var body struct {
					Data struct {
						Attributes    map[string]any `json:"attributes"`
						Relationships struct {
							Prices struct {
								Data []struct {
									ID   string `json:"id"`
									Type string `json:"type"`
								} `json:"data"`
							} `json:"prices"`
						} `json:"relationships"`
					} `json:"data"`
					Included []struct {
						ID            string `json:"id"`
						Type          string `json:"type"`
						Relationships struct {
							Territory struct {
								Data struct {
									ID string `json:"id"`
								} `json:"data"`
							} `json:"territory"`
						} `json:"relationships"`
					} `json:"included"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				a := body.Data.Attributes
				if a["offerMode"] != "FREE_TRIAL" || a["numberOfPeriods"] != float64(1) || a["targetSubscriptionPlanType"] != "UPFRONT" || a["autoRenewEnabled"] != renew || a["offerEligibility"] != "REPLACE_INTRO_OFFERS" {
					t.Errorf("incorrect offer: %v", a)
				}
				if len(body.Included) != 2 || len(body.Data.Relationships.Prices.Data) != 2 {
					t.Fatalf("missing territory prices: %+v", body)
				}
				for i, territory := range []string{"CHN", "USA"} {
					price, linkage := body.Included[i], body.Data.Relationships.Prices.Data[i]
					if price.Relationships.Territory.Data.ID != territory || price.ID != linkage.ID || price.Type != "subscriptionOfferCodePrices" || linkage.Type != price.Type {
						t.Errorf("invalid included price: %+v", price)
					}
				}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprintf(w, `{"data":{"type":"subscriptionOfferCodes","id":"created","attributes":{"name":"朋友体验","duration":"ONE_MONTH","offerMode":"FREE_TRIAL","numberOfPeriods":1,"active":true,"autoRenewEnabled":%t}}}`, renew)
			}))
			defer server.Close()
			client, _ := NewClient(Config{BaseURL: server.URL, IssuerID: "issuer", KeyID: "key", SubscriptionID: "monthly", SigningKey: key})
			offer, err := client.CreateFreeOffer(context.Background(), OfferDraft{Name: "朋友体验", Duration: "ONE_MONTH", CustomerEligibilities: []string{"NEW"}, AutoRenewEnabled: renew})
			if err != nil || offer.ID != "created" || offer.AutoRenewEnabled != renew || requests != 4 {
				t.Fatalf("offer=%+v error=%v requests=%d", offer, err, requests)
			}
		})
	}
}

func TestCreateFreeOfferRejectsInvalidTerritoriesBeforeWriting(t *testing.T) {
	for _, fixture := range []string{
		`{"data":[]}`,
		`{"data":[{"type":"territories","id":"CHN"},{"type":"territories","id":"CHN"}]}`,
		`{"data":[{"type":"territories","id":""}]}`,
		`{"data":[{"type":"wrong","id":"CHN"}]}`,
		`{"data":[{"type":"territories","id":"CHN"}],"links":{"next":"https://attacker.invalid/territories"}}`,
		`{"data":[{"type":"territories","id":"CHN"}],"links":{"next":"SAME_HOST_OTHER_PATH"}}`,
	} {
		t.Run(fixture, func(t *testing.T) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("must not write with invalid territories")
					w.WriteHeader(500)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/planAvailabilities") {
					fmt.Fprint(w, `{"data":[{"type":"subscriptionPlanAvailabilities","id":"standard","attributes":{"planType":"UPFRONT"}}]}`)
				} else {
					fmt.Fprint(w, strings.ReplaceAll(fixture, "SAME_HOST_OTHER_PATH", serverURL(r)+"/v1/other"))
				}
			}))
			defer server.Close()
			client, _ := NewClient(Config{BaseURL: server.URL, IssuerID: "issuer", KeyID: "key", SubscriptionID: "monthly", SigningKey: key})
			if _, err := client.CreateFreeOffer(context.Background(), OfferDraft{}); err != ErrInvalid {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestAppleWriteErrorsAndTokens(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		for _, tc := range []struct {
			status int
			want   error
		}{{400, ErrRejected}, {409, ErrRejected}, {422, ErrRejected}, {405, ErrMethodNotAllowed}, {403, ErrForbidden}, {503, ErrUnavailable}} {
			t.Run(fmt.Sprintf("%s/%d", method, tc.status), func(t *testing.T) {
				key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					claims := decodeClaims(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
					if _, ok := claims["scope"]; ok {
						t.Error("write includes unsupported scope")
					}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, `{"errors":[{"detail":"private upstream payload"}]}`)
				}))
				defer server.Close()
				client, _ := NewClient(Config{BaseURL: server.URL, IssuerID: "issuer", KeyID: "key", SubscriptionID: "monthly", SigningKey: key})
				err := client.send(context.Background(), method, "/v1/subscriptionOfferCodes", map[string]any{}, 201, nil)
				if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "private upstream payload") {
					t.Fatalf("error=%v", err)
				}
			})
		}
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
