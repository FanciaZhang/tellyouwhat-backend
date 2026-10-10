package albums

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type appleTestTransport func(*http.Request) (*http.Response, error)

func (f appleTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testAppleKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func testAppleJWKS(key *rsa.PrivateKey, kid string) string {
	value := map[string]any{"keys": []map[string]string{{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}}
	data, _ := json.Marshal(value)
	return string(data)
}
func appleClaims(now time.Time) jwt.MapClaims {
	return jwt.MapClaims{"iss": appleIssuer, "aud": albumAppleAudience, "sub": "apple-subject-not-an-email", "nonce": strings.Repeat("a", 64), "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
}
func appleSigned(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	token.Header["jku"] = "https://attacker.invalid/keys"
	value, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func appleResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestAppleIdentityRequiresSignatureAudienceIssuerTimesAndNonce(t *testing.T) {
	key := testAppleKey(t)
	now := time.Unix(1791660000, 0)
	var requests atomic.Int64
	verifier := NewAppleIdentityVerifier(appleTestTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.URL.String() != appleIssuer+"/auth/keys" {
			t.Error("token-supplied key source trusted")
		}
		return appleResponse(testAppleJWKS(key, "key-1")), nil
	}))
	verifier.now = func() time.Time { return now }
	ctx := context.Background()
	identity, err := verifier.Verify(ctx, appleSigned(t, key, "key-1", appleClaims(now)), strings.Repeat("a", 64))
	if err != nil || identity.Subject != "apple-subject-not-an-email" {
		t.Fatalf("valid identity %+v %v", identity, err)
	}
	for _, test := range []struct {
		name string
		edit func(jwt.MapClaims)
	}{
		{"other app", func(c jwt.MapClaims) { c["aud"] = "cn.tellyouwhat.health" }},
		{"multiple audiences", func(c jwt.MapClaims) { c["aud"] = []string{albumAppleAudience, "other"} }},
		{"wrong issuer", func(c jwt.MapClaims) { c["iss"] = "https://attacker.invalid" }},
		{"expired", func(c jwt.MapClaims) { c["exp"] = now.Add(-time.Minute).Unix() }},
		{"future issued", func(c jwt.MapClaims) { c["iat"] = now.Add(time.Minute).Unix() }},
		{"stale issued", func(c jwt.MapClaims) { c["iat"] = now.Add(-11 * time.Minute).Unix() }},
		{"missing issued", func(c jwt.MapClaims) { delete(c, "iat") }},
		{"missing expiry", func(c jwt.MapClaims) { delete(c, "exp") }},
		{"missing subject", func(c jwt.MapClaims) { delete(c, "sub") }},
		{"wrong nonce", func(c jwt.MapClaims) { c["nonce"] = strings.Repeat("b", 64) }},
		{"missing nonce", func(c jwt.MapClaims) { delete(c, "nonce") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := appleClaims(now)
			test.edit(claims)
			identity, err := verifier.Verify(ctx, appleSigned(t, key, "key-1", claims), strings.Repeat("a", 64))
			if !errors.Is(err, ErrAppleIdentity) || identity.Subject != "" {
				t.Fatalf("invalid identity accepted: %+v %v", identity, err)
			}
		})
	}
	other := testAppleKey(t)
	if _, err := verifier.Verify(ctx, appleSigned(t, other, "key-1", appleClaims(now)), strings.Repeat("a", 64)); !errors.Is(err, ErrAppleIdentity) {
		t.Fatal("forged signature accepted")
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, appleClaims(now))
	token.Header["kid"] = "key-1"
	confused, _ := token.SignedString([]byte("secret"))
	if _, err := verifier.Verify(ctx, confused, strings.Repeat("a", 64)); !errors.Is(err, ErrAppleIdentity) {
		t.Fatal("algorithm confusion accepted")
	}
	if requests.Load() != 1 {
		t.Fatalf("valid cache refetched %d times", requests.Load())
	}
}

func TestAppleKeysThrottleUnknownKidsAndRefreshRotation(t *testing.T) {
	first, second := testAppleKey(t), testAppleKey(t)
	now := time.Unix(1791660000, 0)
	var requests atomic.Int64
	verifier := NewAppleIdentityVerifier(appleTestTransport(func(*http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return appleResponse(testAppleJWKS(first, "first")), nil
		}
		return appleResponse(testAppleJWKS(second, "second")), nil
	}))
	verifier.now = func() time.Time { return now }
	if _, err := verifier.key(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			if _, err := verifier.key(context.Background(), fmt.Sprintf("unknown-%d", i)); err == nil {
				t.Error("unknown key accepted")
			}
		}(i)
	}
	group.Wait()
	if requests.Load() != 1 {
		t.Fatal("unknown kids triggered outbound flood")
	}
	now = now.Add(31 * time.Second)
	if _, err := verifier.key(context.Background(), "second"); err != nil {
		t.Fatal("rotation was not fetched", err)
	}
	if requests.Load() != 2 {
		t.Fatal("rotation did not refresh")
	}
	if _, err := verifier.key(context.Background(), "first"); err == nil {
		t.Fatal("removed signing key remained valid")
	}
}

func TestAppleKeyFetchRejectsRedirectOversizeAndExpiredCache(t *testing.T) {
	key := testAppleKey(t)
	now := time.Unix(1791660000, 0)
	var count int
	verifier := NewAppleIdentityVerifier(appleTestTransport(func(*http.Request) (*http.Response, error) {
		count++
		if count == 1 {
			return appleResponse(testAppleJWKS(key, "key")), nil
		}
		return nil, errors.New("provider unavailable with sensitive detail")
	}))
	verifier.now = func() time.Time { return now }
	if _, err := verifier.key(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour + time.Second)
	if _, err := verifier.key(context.Background(), "key"); !errors.Is(err, ErrAppleIdentity) {
		t.Fatalf("stale keys or raw errors escaped: %v", err)
	}
	for _, mode := range []string{"redirect", "oversize", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			v := NewAppleIdentityVerifier(appleTestTransport(func(*http.Request) (*http.Response, error) {
				response := appleResponse("invalid")
				if mode == "redirect" {
					response.StatusCode = 302
					response.Header.Set("Location", "https://attacker.invalid")
				}
				if mode == "oversize" {
					response = appleResponse(strings.Repeat("x", (64<<10)+1))
				}
				return response, nil
			}))
			if _, err := v.key(context.Background(), "key"); !errors.Is(err, ErrAppleIdentity) {
				t.Fatal("invalid key response accepted")
			}
		})
	}
}
