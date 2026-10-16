package albums

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func testAppleRefreshRevocation(t *testing.T, bundleID string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := AppleCodeConfig{BundleID: bundleID, TeamID: "TEAMID1234", KeyID: "KEYID12345", PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}
	for _, mode := range []string{"accepted", "repeat", "empty-token", "oversized-token", "invalid-token", "cancelled", "redirect", "bad-status", "unexpected-body", "transport-failure"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			transport := appleTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != appleIssuer+"/auth/revoke" || r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
					t.Fatal("wrong revoke endpoint")
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if len(r.Form) != 4 || r.Form.Get("client_id") != bundleID || r.Form.Get("client_secret") == "" || r.Form.Get("token") != "private+&refresh" || r.Form.Get("token_type_hint") != "refresh_token" {
					t.Fatal("wrong native revoke form")
				}
				response := appleResponse("")
				switch mode {
				case "redirect":
					response.StatusCode = 302
					response.Header.Set("Location", "https://attacker.invalid")
				case "bad-status":
					response = appleResponse(`{"error":"private-provider-detail"}`)
					response.StatusCode = 400
				case "unexpected-body":
					response = appleResponse(strings.Repeat("private-provider-detail", 1000))
				case "transport-failure":
					return nil, errors.New("private-provider-detail")
				}
				return response, nil
			})
			e, err := NewAppleCodeExchanger(config, NewAppleIdentityVerifier(bundleID, transport), transport)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			token := "private+&refresh"
			switch mode {
			case "empty-token":
				token = ""
			case "oversized-token":
				token = strings.Repeat("x", (16<<10)+1)
			case "invalid-token":
				token = "private\nrefresh"
			case "cancelled":
				cancel()
			}
			err = e.revokeRefreshCredential(ctx, token)
			if mode == "accepted" || mode == "repeat" {
				if err != nil {
					t.Fatal("valid acknowledgement refused", err)
				}
				if mode == "repeat" {
					if err = e.revokeRefreshCredential(ctx, token); err != nil {
						t.Fatal("safe explicit retry failed", err)
					}
				}
			} else if !errors.Is(err, errAppleRevocation) || strings.Contains(err.Error(), "private-") {
				t.Fatal("invalid acknowledgement or raw provider failure")
			}
			expected := 1
			switch mode {
			case "repeat":
				expected = 2
			case "empty-token", "oversized-token", "invalid-token", "cancelled":
				expected = 0
			}
			if calls != expected {
				t.Fatal("redirect, invalid request, or automatic retry", calls)
			}
		})
	}
}

func TestAppleRefreshRevocationFixedEndpointAndBoundedAcknowledgement(t *testing.T) {
	for _, bundleID := range []string{albumAppleAudience, albumDevelopmentAudience} {
		t.Run(bundleID, func(t *testing.T) { testAppleRefreshRevocation(t, bundleID) })
	}
}
