package albums

import (
	"context"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const albumAppleAudience = "cn.tellyouwhat.albums"
const appleIssuer = "https://appleid.apple.com"

var ErrAppleIdentity = errors.New("Apple identity could not be verified")
var loginNoncePattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// AppleIdentity is a verified provider identity, not an internal account UUID.
// Account linking must use issuer/audience/subject, never name or email.
type AppleIdentity struct{ Subject string }

type appleIdentityClaims struct {
	jwt.RegisteredClaims
	Nonce string `json:"nonce"`
}

// AppleIdentityVerifier only authenticates the signed identity assertion.
// A login service must additionally consume its stored, single-use challenge
// and validate the authorization code before issuing a revocable app session.
type AppleIdentityVerifier struct {
	client    *http.Client
	now       func() time.Time
	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	expires   time.Time
	lastFetch time.Time
}

func NewAppleIdentityVerifier(transport http.RoundTripper) *AppleIdentityVerifier {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &AppleIdentityVerifier{
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		now: time.Now,
	}
}

func (v *AppleIdentityVerifier) Verify(ctx context.Context, encoded, expectedNonce string) (AppleIdentity, error) {
	if len(encoded) == 0 || len(encoded) > 16<<10 || !loginNoncePattern.MatchString(expectedNonce) {
		return AppleIdentity{}, ErrAppleIdentity
	}
	now := v.now()
	claims := &appleIdentityClaims{}
	token, err := jwt.ParseWithClaims(encoded, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodRS256 {
			return nil, ErrAppleIdentity
		}
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" || len(kid) > 128 {
			return nil, ErrAppleIdentity
		}
		// Ignore token-supplied jku/x5u URLs. The only key source is Apple's endpoint.
		return v.key(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(appleIssuer),
		jwt.WithAudience(albumAppleAudience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithLeeway(30*time.Second), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || token == nil || !token.Valid || claims.IssuedAt == nil || claims.ExpiresAt == nil ||
		claims.IssuedAt.Time.Before(now.Add(-10*time.Minute)) || !claims.ExpiresAt.Time.After(claims.IssuedAt.Time) ||
		len(claims.Audience) != 1 || claims.Subject == "" || len(claims.Subject) > 255 ||
		subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(expectedNonce)) != 1 {
		return AppleIdentity{}, ErrAppleIdentity
	}
	return AppleIdentity{Subject: claims.Subject}, nil
}

func (v *AppleIdentityVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ErrAppleIdentity
	}
	now := v.now()
	if key := v.keys[kid]; key != nil && now.Before(v.expires) {
		return key, nil
	}
	// Unknown kids must not permit one outbound request per unauthenticated token.
	if !v.lastFetch.IsZero() && now.Sub(v.lastFetch) < 30*time.Second {
		return nil, ErrAppleIdentity
	}
	v.lastFetch = now
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, appleIssuer+"/auth/keys", nil)
	if err != nil {
		return nil, ErrAppleIdentity
	}
	response, err := v.client.Do(request)
	if err != nil {
		return nil, ErrAppleIdentity
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrAppleIdentity
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, ErrAppleIdentity
	}
	keys, err := decodeAppleKeys(data)
	if err != nil {
		return nil, ErrAppleIdentity
	}
	v.keys = keys
	v.expires = now.Add(time.Hour)
	if key := keys[kid]; key != nil {
		return key, nil
	}
	return nil, ErrAppleIdentity
}

func decodeAppleKeys(data []byte) (map[string]*rsa.PublicKey, error) {
	var document struct {
		Keys []struct{ Kty, Kid, Use, Alg, N, E string } `json:"keys"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Keys) == 0 || len(document.Keys) > 16 {
		return nil, ErrAppleIdentity
	}
	keys := make(map[string]*rsa.PublicKey, len(document.Keys))
	for _, key := range document.Keys {
		if key.Kty != "RSA" || key.Use != "sig" || key.Alg != "RS256" {
			continue
		}
		if key.Kid == "" || len(key.Kid) > 128 || keys[key.Kid] != nil {
			return nil, ErrAppleIdentity
		}
		n, err := base64.RawURLEncoding.DecodeString(key.N)
		if err != nil {
			return nil, ErrAppleIdentity
		}
		e, err := base64.RawURLEncoding.DecodeString(key.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, ErrAppleIdentity
		}
		modulus := new(big.Int).SetBytes(n)
		exponent := new(big.Int).SetBytes(e).Int64()
		if modulus.BitLen() < 2048 || modulus.BitLen() > 8192 || modulus.Bit(0) != 1 || exponent < 3 || exponent > 2147483647 || exponent%2 == 0 {
			return nil, ErrAppleIdentity
		}
		keys[key.Kid] = &rsa.PublicKey{N: modulus, E: int(exponent)}
	}
	if len(keys) == 0 {
		return nil, ErrAppleIdentity
	}
	return keys, nil
}
