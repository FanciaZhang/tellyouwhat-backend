package albums

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAppleCodeExchangeBindsCodeToBothSignedAssertions(t *testing.T) {
	appleKey := testAppleKey(t)
	signing, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(signing)
	if err != nil {
		t.Fatal(err)
	}
	config := AppleCodeConfig{TeamID: "TEAMID1234", KeyID: "KEYID12345", PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}
	now := time.Unix(1791660000, 0)
	original := appleSigned(t, appleKey, "apple-key", appleClaims(now))
	for _, mode := range []string{"valid", "other subject", "other nonce", "expired returned", "invalid client", "provider error", "oversize", "redirect", "transport failure"} {
		t.Run(mode, func(t *testing.T) {
			codeRequests := 0
			transport := appleTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == appleIssuer+"/auth/keys" {
					return appleResponse(testAppleJWKS(appleKey, "apple-key")), nil
				}
				if r.URL.String() != appleIssuer+"/auth/token" || r.Method != "POST" {
					t.Fatal("unexpected token endpoint")
				}
				codeRequests++
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if r.Form.Get("client_id") != albumAppleAudience || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "one-use-code+&" || len(r.Form) != 4 {
					t.Error("incorrect native code request")
				}
				secret, err := jwt.ParseWithClaims(r.Form.Get("client_secret"), &jwt.RegisteredClaims{}, func(token *jwt.Token) (any, error) {
					if token.Header["kid"] != config.KeyID {
						t.Error("incorrect client secret key ID")
					}
					return &signing.PublicKey, nil
				}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(config.TeamID), jwt.WithAudience(appleIssuer), jwt.WithSubject(albumAppleAudience), jwt.WithExpirationRequired(), jwt.WithTimeFunc(func() time.Time { return now }))
				if err != nil || !secret.Valid {
					t.Fatal("client secret failed signature/claims verification", err)
				}
				claims := secret.Claims.(*jwt.RegisteredClaims)
				if claims.IssuedAt == nil || claims.ExpiresAt.Sub(claims.IssuedAt.Time) != 5*time.Minute {
					t.Error("client secret lifetime was not bounded")
				}
				if mode == "transport failure" {
					return nil, errors.New("private-provider-detail")
				}
				if mode == "redirect" {
					response := appleResponse("")
					response.StatusCode = 302
					response.Header.Set("Location", "https://attacker.invalid")
					return response, nil
				}
				if mode == "oversize" {
					return appleResponse(strings.Repeat("x", (64<<10)+1)), nil
				}
				if mode == "provider error" {
					return appleResponse(`{"error":"invalid_grant","error_description":"private-provider-detail"}`), nil
				}
				returned := appleClaims(now)
				if mode == "other subject" {
					returned["sub"] = "different-user"
				}
				if mode == "other nonce" {
					returned["nonce"] = strings.Repeat("b", 64)
				}
				if mode == "expired returned" {
					returned["exp"] = now.Add(-time.Minute).Unix()
				}
				data, _ := json.Marshal(map[string]string{"id_token": appleSigned(t, appleKey, "apple-key", returned), "refresh_token": "private-refresh-token", "token_type": "Bearer"})
				return appleResponse(string(data)), nil
			})
			verifier := NewAppleIdentityVerifier(transport)
			verifier.now = func() time.Time { return now }
			exchanger, err := NewAppleCodeExchanger(config, verifier, transport)
			if err != nil {
				t.Fatal(err)
			}
			exchanger.now = func() time.Time { return now }
			input := original
			if mode == "invalid client" {
				input = "not-a-token"
			}
			grant, err := exchanger.Exchange(context.Background(), "one-use-code+&", input, strings.Repeat("a", 64))
			if mode == "valid" {
				if err != nil || grant.Identity.Subject != "apple-subject-not-an-email" || grant.RefreshToken != "private-refresh-token" {
					t.Fatal("valid code exchange failed", err)
				}
				serialized, _ := json.Marshal(grant)
				if strings.Contains(string(serialized), "private-refresh-token") || strings.Contains(fmt.Sprintf("%v %+v %#v", grant, grant, grant), "private-refresh-token") {
					t.Fatal("refresh credential exposed")
				}
			} else {
				if !errors.Is(err, ErrAppleCode) || grant.RefreshToken != "" || grant.Identity.Subject != "" {
					t.Fatal("invalid code exchange produced a grant")
				}
				if strings.Contains(err.Error(), "private-") {
					t.Fatal("raw provider error escaped")
				}
			}
			expected := 1
			if mode == "invalid client" {
				expected = 0
			}
			if codeRequests != expected {
				t.Fatalf("code requests=%d want=%d; code was retried or invalid client consumed it", codeRequests, expected)
			}
		})
	}
}

func TestAppleCodeConfigurationRejectsWrongCurveAndMissingVerifier(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	config := AppleCodeConfig{TeamID: "TEAMID1234", KeyID: "KEYID12345", PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}
	if _, err := NewAppleCodeExchanger(config, NewAppleIdentityVerifier(nil), nil); err == nil {
		t.Fatal("wrong curve accepted")
	}
	if _, err := NewAppleCodeExchanger(config, nil, nil); err == nil {
		t.Fatal("missing identity verifier accepted")
	}
}

func TestLoginChallengeRemainsConsumedAfterUncertainAppleExchange(t *testing.T) {
	signing, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(signing)
	key := testAppleKey(t)
	now := time.Unix(1791660000, 0)
	store := &challengeTestStore{rows: map[string]StoredLoginChallenge{}, used: map[string]bool{}}
	service, _ := NewLoginChallengeService(store, func() time.Time { return now })
	challenge, err := service.Issue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	claims := appleClaims(now)
	claims["nonce"] = challenge.Nonce
	assertion := appleSigned(t, key, "key", claims)
	requests := 0
	transport := appleTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/auth/keys" {
			return appleResponse(testAppleJWKS(key, "key")), nil
		}
		requests++
		return nil, errors.New("connection lost after authorization code sent")
	})
	verifier := NewAppleIdentityVerifier(transport)
	verifier.now = func() time.Time { return now }
	exchanger, err := NewAppleCodeExchanger(AppleCodeConfig{TeamID: "TEAMID1234", KeyID: "KEYID12345", PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}, verifier, transport)
	if err != nil {
		t.Fatal(err)
	}
	exchanger.now = func() time.Time { return now }
	if _, err := service.VerifyAppleLogin(context.Background(), exchanger, challenge.ID, challenge.Proof, "code", assertion); !errors.Is(err, ErrAppleCode) {
		t.Fatal("unknown exchange did not fail closed")
	}
	if _, err := service.VerifyAppleLogin(context.Background(), exchanger, challenge.ID, challenge.Proof, "code", assertion); !errors.Is(err, ErrLoginChallenge) {
		t.Fatal("used challenge retried")
	}
	if requests != 1 {
		t.Fatalf("single-use authorization code sent %d times", requests)
	}
}
