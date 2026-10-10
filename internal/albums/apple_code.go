package albums

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var ErrAppleCode = errors.New("Apple authorization code could not be verified")
var appleIdentifierPattern = regexp.MustCompile(`^[A-Z0-9]{10}$`)

type AppleCodeConfig struct {
	TeamID        string
	KeyID         string
	PrivateKeyPEM []byte
}

type AppleGrant struct {
	Identity     AppleIdentity
	RefreshToken string `json:"-"`
}

func (AppleGrant) String() string       { return "[redacted Apple grant]" }
func (AppleGrant) GoString() string     { return "[redacted Apple grant]" }
func (AppleGrant) LogValue() slog.Value { return slog.StringValue("[redacted Apple grant]") }

// AppleCodeExchanger is for the native app's code flow. The client_id is fixed;
// web redirect_uri and caller-supplied endpoint/client IDs are not accepted.
// RefreshToken must be encrypted at rest by the account store, never returned
// to the app or used as the app's bearer session.
type AppleCodeExchanger struct {
	teamID, keyID string
	key           *ecdsa.PrivateKey
	verifier      *AppleIdentityVerifier
	client        *http.Client
	now           func() time.Time
}

func NewAppleCodeExchanger(config AppleCodeConfig, verifier *AppleIdentityVerifier, transport http.RoundTripper) (*AppleCodeExchanger, error) {
	if !appleIdentifierPattern.MatchString(config.TeamID) || !appleIdentifierPattern.MatchString(config.KeyID) || len(config.PrivateKeyPEM) > 16<<10 || verifier == nil {
		return nil, errors.New("invalid Apple code exchange configuration")
	}
	key, err := jwt.ParseECPrivateKeyFromPEM(config.PrivateKeyPEM)
	if err != nil || key.Curve != elliptic.P256() {
		return nil, errors.New("invalid Apple signing key")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &AppleCodeExchanger{teamID: config.TeamID, keyID: config.KeyID, key: key, verifier: verifier, now: time.Now,
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (e *AppleCodeExchanger) clientSecret() (string, error) {
	now := e.now().UTC()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.RegisteredClaims{
		Issuer: e.teamID, Subject: albumAppleAudience, Audience: jwt.ClaimStrings{appleIssuer},
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
	})
	token.Header["kid"] = e.keyID
	return token.SignedString(e.key)
}

func (e *AppleCodeExchanger) Exchange(ctx context.Context, code, identityToken, expectedNonce string) (AppleGrant, error) {
	if code == "" || len(code) > 4096 || strings.ContainsAny(code, "\r\n") {
		return AppleGrant{}, ErrAppleCode
	}
	// Check the client assertion before consuming Apple's single-use code.
	identity, err := e.verifier.Verify(ctx, identityToken, expectedNonce)
	if err != nil {
		return AppleGrant{}, ErrAppleCode
	}
	secret, err := e.clientSecret()
	if err != nil {
		return AppleGrant{}, ErrAppleCode
	}
	form := url.Values{"client_id": {albumAppleAudience}, "client_secret": {secret}, "code": {code}, "grant_type": {"authorization_code"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, appleIssuer+"/auth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return AppleGrant{}, ErrAppleCode
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := e.client.Do(request)
	// Do not automatically retry: a transport failure may have consumed the code.
	if err != nil {
		return AppleGrant{}, ErrAppleCode
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return AppleGrant{}, ErrAppleCode
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return AppleGrant{}, ErrAppleCode
	}
	var result struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		Error        string `json:"error"`
	}
	if json.Unmarshal(data, &result) != nil || result.Error != "" || result.TokenType != "Bearer" || result.RefreshToken == "" || len(result.RefreshToken) > 16<<10 {
		return AppleGrant{}, ErrAppleCode
	}
	verified, err := e.verifier.Verify(ctx, result.IDToken, expectedNonce)
	if err != nil || verified.Subject != identity.Subject {
		return AppleGrant{}, ErrAppleCode
	}
	return AppleGrant{Identity: verified, RefreshToken: result.RefreshToken}, nil
}
